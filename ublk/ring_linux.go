//go:build linux && (amd64 || arm64)

package ublk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

var le = binary.LittleEndian

const setupCoopTaskrun uint32 = 1 << 8

// ring is a single-producer/single-consumer io_uring, with 128-byte SQEs and
// 32-byte CQEs. All kernel-retained pointers refer to mmap memory, never Go's heap.
// ABI: include/uapi/linux/io_uring.h. Commands serve ublk; POLL_ADD wakes the
// concurrent queue when backend workers publish their completions.
type ring struct {
	fd                                 int
	sq, cq, sqes                       []byte
	shared                             bool
	sqHead, sqTail, cqHead, cqTail     *uint32
	sqMask, cqMask, sqArray, cqeOffset uint32
	entries                            uint32
}

func newRing(entries, flags uint32) (r *ring, err error) {
	var params [120]byte
	le.PutUint32(params[8:], flags|1<<10|1<<11)
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	r = &ring{fd: int(fd)}
	defer func() {
		if err != nil {
			r.close()
			r = nil
		}
	}()
	r.entries = le.Uint32(params[:])
	sqOffset, cqOffset := params[40:80], params[80:120]
	sqSize := le.Uint32(sqOffset[24:]) + r.entries*4
	cqSize := le.Uint32(cqOffset[20:]) + le.Uint32(params[4:])*32
	r.shared = le.Uint32(params[20:])&1 != 0
	if r.shared {
		sqSize = max(sqSize, cqSize)
	}
	r.sq, err = unix.Mmap(r.fd, 0, int(sqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return r, err
	}
	if r.shared {
		r.cq = r.sq
	} else {
		r.cq, err = unix.Mmap(r.fd, 0x8000000, int(cqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return r, err
		}
	}
	r.sqes, err = unix.Mmap(r.fd, 0x10000000, int(r.entries)*128, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return r, err
	}
	r.sqHead = word(r.sq, le.Uint32(sqOffset))
	r.sqTail = word(r.sq, le.Uint32(sqOffset[4:]))
	r.cqHead = word(r.cq, le.Uint32(cqOffset))
	r.cqTail = word(r.cq, le.Uint32(cqOffset[4:]))
	r.sqMask = le.Uint32(r.sq[le.Uint32(sqOffset[8:]):])
	r.cqMask = le.Uint32(r.cq[le.Uint32(cqOffset[8:]):])
	r.sqArray = le.Uint32(sqOffset[24:])
	r.cqeOffset = le.Uint32(cqOffset[20:])
	return r, nil
}

func word(b []byte, off uint32) *uint32 { return (*uint32)(unsafe.Pointer(&b[off])) }
func address(b []byte) uint64           { return uint64(uintptr(unsafe.Pointer(&b[0]))) }
func anon(size int) ([]byte, error) {
	return unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
}

func (r *ring) enqueue(fd int, op uint32, cmd []byte, tag uint64) error {
	tail := atomic.LoadUint32(r.sqTail)
	if tail-atomic.LoadUint32(r.sqHead) >= r.entries {
		return errors.New("io_uring: submission queue full")
	}
	idx := tail & r.sqMask
	sqe := r.sqes[idx*128 : (idx+1)*128]
	clear(sqe)
	sqe[0] = 46 // IORING_OP_URING_CMD
	le.PutUint32(sqe[4:], uint32(fd))
	le.PutUint32(sqe[8:], op)
	le.PutUint64(sqe[32:], tag)
	copy(sqe[48:], cmd)
	le.PutUint32(r.sq[r.sqArray+idx*4:], idx)
	atomic.StoreUint32(r.sqTail, tail+1)
	return nil
}

// enqueuePoll wakes the queue for completed backend work while fetch commands
// remain in flight. The eventfd itself is owned by runConcurrent.
func (r *ring) enqueuePoll(fd int, tag uint64) error {
	tail := atomic.LoadUint32(r.sqTail)
	if tail-atomic.LoadUint32(r.sqHead) >= r.entries {
		return errors.New("io_uring: submission queue full")
	}
	idx := tail & r.sqMask
	sqe := r.sqes[idx*128 : (idx+1)*128]
	clear(sqe)
	sqe[0] = 6 // IORING_OP_POLL_ADD
	le.PutUint32(sqe[4:], uint32(fd))
	le.PutUint32(sqe[28:], unix.POLLIN)
	le.PutUint64(sqe[32:], tag)
	le.PutUint32(r.sq[r.sqArray+idx*4:], idx)
	atomic.StoreUint32(r.sqTail, tail+1)
	return nil
}

func (r *ring) enter(wait bool) error {
	for {
		pending := r.pending()
		var minimum, flags uintptr
		if wait {
			minimum, flags = 1, 1
		}
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd), uintptr(pending), minimum, flags, 0, 0)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}
		if atomic.LoadUint32(r.sqTail) != atomic.LoadUint32(r.sqHead) {
			continue
		}
		return nil
	}
}

func (r *ring) pending() uint32 {
	return atomic.LoadUint32(r.sqTail) - atomic.LoadUint32(r.sqHead)
}

func (r *ring) ready() bool {
	return atomic.LoadUint32(r.cqHead) != atomic.LoadUint32(r.cqTail)
}

// readyExcept peeks at a fixed CQ snapshot without consuming completions.
// The queue owns cqHead; ignored wake notifications remain for its handler.
func (r *ring) readyExcept(ignoredTag uint64) bool {
	head, tail := atomic.LoadUint32(r.cqHead), atomic.LoadUint32(r.cqTail)
	for ; head != tail; head++ {
		off := r.cqeOffset + (head&r.cqMask)*32
		if le.Uint64(r.cq[off:]) != ignoredTag {
			return true
		}
	}
	return false
}

func (r *ring) completion() (uint64, int32, error) {
	for {
		head := atomic.LoadUint32(r.cqHead)
		if head != atomic.LoadUint32(r.cqTail) {
			off := r.cqeOffset + (head&r.cqMask)*32
			tag, res := le.Uint64(r.cq[off:]), int32(le.Uint32(r.cq[off+8:]))
			atomic.StoreUint32(r.cqHead, head+1)
			return tag, res, nil
		}
		if err := r.enter(true); err != nil {
			return 0, 0, err
		}
	}
}

func (r *ring) close() {
	if r == nil {
		return
	}
	unix.Close(r.fd)
	if r.sqes != nil {
		unix.Munmap(r.sqes)
	}
	if r.cq != nil && !r.shared {
		unix.Munmap(r.cq)
	}
	if r.sq != nil {
		unix.Munmap(r.sq)
	}
}
