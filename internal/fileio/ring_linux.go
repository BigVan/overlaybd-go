//go:build linux && (amd64 || arm64)

package fileio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	opRead      = 22
	opWrite     = 23
	opFsync     = 3
	coopTaskrun = 1 << 8
)

var little = binary.LittleEndian

// A ring has one borrower and one outstanding operation. Submission and waiting
// happen in the caller, with no worker goroutine or channel handoff per request.
// SQ/CQ memory and optional alignment scratch space are mmap-backed.
type ring struct {
	fd                                int
	sq, cq, sqes, scratch             []byte
	shared                            bool
	sqHead, sqTail, cqHead, cqTail    *uint32
	sqMask, cqMask, sqArray, cqOffset uint32
	sequence                          uint64
	broken                            error
	executor                          *executor
	request                           *request
	writerRequest                     *request
}

func openRing() (*ring, error) { return openRingSize(2) }

func openRingSize(entries uintptr) (r *ring, err error) { return openRingFlags(entries, coopTaskrun) }

func openRingFlags(entries uintptr, flags uint32) (r *ring, err error) {
	var params [120]byte
	little.PutUint32(params[8:], flags)
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, entries, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno == unix.EINVAL {
		// COOP_TASKRUN predates the ublk minimum, but this package can also be
		// used on older kernels. Both paths still perform real io_uring I/O.
		clear(params[:])
		fd, _, errno = unix.Syscall(unix.SYS_IO_URING_SETUP, entries, uintptr(unsafe.Pointer(&params[0])), 0)
	}
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	r = &ring{fd: int(fd)}
	defer func() {
		if err != nil {
			_ = r.close()
			r = nil
		}
	}()
	s, c := params[40:80], params[80:120]
	sqBytes := little.Uint32(s[24:]) + little.Uint32(params[:])*4
	cqBytes := little.Uint32(c[20:]) + little.Uint32(params[4:])*16
	r.shared = little.Uint32(params[20:])&1 != 0
	if r.shared {
		sqBytes = max(sqBytes, cqBytes)
	}
	r.sq, err = unix.Mmap(r.fd, 0, int(sqBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return r, err
	}
	if r.shared {
		r.cq = r.sq
	} else {
		r.cq, err = unix.Mmap(r.fd, 0x8000000, int(cqBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return r, err
		}
	}
	r.sqes, err = unix.Mmap(r.fd, 0x10000000, int(little.Uint32(params[:]))*64, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return r, err
	}
	word := func(b []byte, off uint32) *uint32 { return (*uint32)(unsafe.Pointer(&b[off])) }
	r.sqHead, r.sqTail = word(r.sq, little.Uint32(s)), word(r.sq, little.Uint32(s[4:]))
	r.cqHead, r.cqTail = word(r.cq, little.Uint32(c)), word(r.cq, little.Uint32(c[4:]))
	r.sqMask = little.Uint32(r.sq[little.Uint32(s[8:]):])
	r.cqMask = little.Uint32(r.cq[little.Uint32(c[8:]):])
	r.sqArray, r.cqOffset = little.Uint32(s[24:]), little.Uint32(c[20:])
	return r, nil
}

func (r *ring) buffer(n int) ([]byte, error) {
	if len(r.scratch) < n {
		b, err := unix.Mmap(-1, 0, (n+alignment-1)&^(alignment-1), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
		if err != nil {
			return nil, err
		}
		if r.scratch != nil {
			if err = unix.Munmap(r.scratch); err != nil {
				_ = unix.Munmap(b)
				return nil, err
			}
		}
		r.scratch = b
	}
	return r.scratch[:n], nil
}

// run keeps Go-owned data pinned until its CQE has been consumed. Pinner ignores
// non-Go pointers, including the mmap buffers supplied by ublk and this ring.
func (r *ring) run(fd int, op byte, b []byte, off int64) (int, error) {
	if r.executor != nil {
		return r.executor.run(r.request, fd, op, b, off)
	}
	if r.broken != nil {
		return 0, r.broken
	}
	// io_uring_setup(2) excludes different submitting/waiting threads from
	// COOP_TASKRUN's safe usage. Keep this operation on its submitting thread
	// even when enter is interrupted and Go would otherwise migrate it.
	// Rings may be borrowed by other threads after the CQE is consumed.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var pin runtime.Pinner
	var address uint64
	if len(b) != 0 {
		pin.Pin(&b[0])
		defer pin.Unpin()
		address = uint64(uintptr(unsafe.Pointer(&b[0])))
	}
	tail := r.enqueue(fd, op, address, len(b), off)
	cancelled := false
	for {
		head := atomic.LoadUint32(r.cqHead)
		if head != atomic.LoadUint32(r.cqTail) {
			at := r.cqOffset + (head&r.cqMask)*16
			result := int32(little.Uint32(r.cq[at+8:]))
			atomic.StoreUint32(r.cqHead, head+1)
			runtime.KeepAlive(b)
			if result < 0 {
				return 0, errors.Join(r.broken, unix.Errno(-result))
			}
			return int(result), r.broken
		}
		pending := atomic.LoadUint32(r.sqTail) - atomic.LoadUint32(r.sqHead)
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd), uintptr(pending), 1, 1, 0, 0)
		if errno == 0 || errno == unix.EINTR || errno == unix.EAGAIN || errno == unix.EBUSY {
			continue
		}
		if r.broken == nil {
			r.broken = fmt.Errorf("io_uring_enter: %w", errno)
		}
		if atomic.LoadUint32(r.sqHead) == tail {
			// No SQPOLL: an unconsumed SQE cannot be picked up after enter
			// returns. Withdraw it before allowing the caller to unpin data.
			atomic.StoreUint32(r.sqTail, tail)
			return 0, r.broken
		}
		// A consumed SQE may still retain the pointer despite enter failing.
		// Request synchronous cancellation once, then drain the CQE before
		// unpinning. Older kernels can reject sync-cancel; keep waiting there.
		// The private ring descriptor cannot be closed by File.Close while
		// this operation holds File.life. Never unpin merely after close(fd).
		if !cancelled {
			cancelled = true
			if err := r.cancelCurrent(); err != nil {
				r.broken = errors.Join(r.broken, err)
			}
		}
		// An exceptional repeated error must not become a CPU spin loop.
		_ = unix.Nanosleep(&unix.Timespec{Nsec: 1_000_000}, nil)
	}
}

// enqueue is called by the ring's sole borrower. No SQPOLL thread consumes it.
func (r *ring) enqueue(fd int, op byte, address uint64, length int, off int64) uint32 {
	r.sequence++
	return r.enqueueTag(fd, op, address, length, off, r.sequence)
}

func (r *ring) enqueueTag(fd int, op byte, address uint64, length int, off int64, tag uint64) uint32 {
	tail := atomic.LoadUint32(r.sqTail)
	idx := tail & r.sqMask
	sqe := r.sqes[idx*64 : (idx+1)*64]
	clear(sqe)
	sqe[0] = op
	little.PutUint32(sqe[4:], uint32(fd))
	little.PutUint64(sqe[8:], uint64(off))
	little.PutUint64(sqe[16:], address)
	little.PutUint32(sqe[24:], uint32(length))
	little.PutUint64(sqe[32:], tag)
	little.PutUint32(r.sq[r.sqArray+idx*4:], idx)
	atomic.StoreUint32(r.sqTail, tail+1)
	return tail
}

func (r *ring) cancelCurrent() error {
	// IORING_REGISTER_SYNC_CANCEL, matching the single request's user_data.
	// Both timeout fields -1 request an unbounded cancellation wait.
	var request [64]byte
	little.PutUint64(request[:], r.sequence)
	little.PutUint32(request[8:], ^uint32(0))
	little.PutUint64(request[16:], ^uint64(0))
	little.PutUint64(request[24:], ^uint64(0))
	for {
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(r.fd), 24, uintptr(unsafe.Pointer(&request[0])), 1, 0, 0)
		if errno == unix.EINTR {
			continue
		}
		if errno == 0 || errno == unix.ENOENT || errno == unix.EALREADY {
			return nil
		}
		return fmt.Errorf("io_uring sync cancel: %w", errno)
	}
}

func (r *ring) close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.fd >= 0 {
		err = unix.Close(r.fd)
	}
	if r.scratch != nil {
		err = errors.Join(err, unix.Munmap(r.scratch))
	}
	if r.sqes != nil {
		err = errors.Join(err, unix.Munmap(r.sqes))
	}
	if r.cq != nil && !r.shared {
		err = errors.Join(err, unix.Munmap(r.cq))
	}
	if r.sq != nil {
		err = errors.Join(err, unix.Munmap(r.sq))
	}
	return err
}
