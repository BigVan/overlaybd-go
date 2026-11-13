//go:build linux && (amd64 || arm64)

package fileio

import (
	"cmp"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

type request struct {
	id     uint64
	fd     int
	op     byte
	buffer []byte
	offset int64
	result int
	err    error
	pin    runtime.Pinner
	done   chan struct{}
}

type executorError struct{ error }

const opWritev = 2
const vectorBytes = maxRings * 16

// A group owns its members and mmap iovecs until its single CQE is consumed.
// Each member remains pinned and unavailable to its client independently.
type requestGroup struct {
	members [maxRings]*request
	count   int
}

func alignedWrite(q *request) bool {
	return q.op == opWrite && len(q.buffer) > 0 && len(q.buffer) <= maxTransfer &&
		q.offset >= 0 && q.offset%alignment == 0 && len(q.buffer)%alignment == 0 && aligned(q.buffer)
}

func writeGroupEnd(batch []*request, start int) int {
	q := batch[start]
	length := len(q.buffer)
	end := start + 1
	for end < len(batch) && end-start < maxRings {
		next := batch[end]
		if next.fd != q.fd || next.offset != q.offset+int64(length) || length+len(next.buffer) > maxTransfer {
			break
		}
		length += len(next.buffer)
		end++
	}
	return end
}

type executor struct {
	ring                               *ring
	inline                             *ring
	running                            atomic.Int32
	boundThread                        atomic.Int32
	event                              int
	incoming                           chan *request
	stopping                           atomic.Bool
	sleeping                           atomic.Bool
	notified                           atomic.Bool
	broken                             atomic.Pointer[executorError]
	done                               chan struct{}
	stop                               chan struct{}
	clients                            atomic.Uint32
	requests, enters, submissions      uint64
	enter                              func(*ring, uint32) unix.Errno
	iovecs                             []byte
	coalescedWrites, coalescedRequests uint64
}

func newExecutor() (*executor, error) { return newExecutorWithInline(openRing) }

func newExecutorWithInline(openInline func() (*ring, error)) (*executor, error) {
	e, err := newExecutorWithEnter(enterRing)
	if err != nil {
		return nil, err
	}
	// A failed ring constructor has already released its partially built ring.
	// Never retain its error-path value and close that descriptor a second time.
	inline, err := openInline()
	if err != nil {
		_ = e.close()
		return nil, err
	}
	e.inline = inline
	return e, nil
}

func enterRing(r *ring, count uint32) unix.Errno {
	_, _, err := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd), uintptr(count), 1, 1, 0, 0)
	return err
}

func newExecutorWithEnter(enter func(*ring, uint32) unix.Errno) (*executor, error) {
	r, err := openRingFlags(128, 0)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		_ = r.close()
		return nil, err
	}
	iovecs, err := unix.Mmap(-1, 0, (maxRings+1)*vectorBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		_ = r.close()
		_ = unix.Close(fd)
		return nil, err
	}
	e := &executor{ring: r, event: fd, incoming: make(chan *request, maxRings), done: make(chan struct{}), stop: make(chan struct{}), enter: enter, iovecs: iovecs}
	go e.loop()
	return e, nil
}

func (e *executor) client(id uint64) *ring {
	e.clients.Store(uint32(id))
	return &ring{fd: -1, executor: e, request: &request{id: id, done: make(chan struct{}, 1)}}
}

func (e *executor) wake() {
	if !e.sleeping.Load() || !e.notified.CompareAndSwap(false, true) {
		return
	}
	var b [8]byte
	little.PutUint64(b[:], 1)
	for {
		_, err := unix.Write(e.event, b[:])
		if err == unix.EINTR {
			continue
		}
		break
	}
}

func (e *executor) run(q *request, fd int, op byte, b []byte, off int64) (int, error) {
	if failure := e.broken.Load(); failure != nil {
		return 0, failure.error
	}
	// Without a thread binding, the first active call owns the inline ring.
	// With a binding, only that OS thread may use it; all workers use shared
	// submission regardless of request count. Count until CQE consumption.
	active := e.running.Add(1)
	defer e.running.Add(-1)
	if e.inline != nil {
		bound := e.boundThread.Load()
		if bound == 0 {
			if active == 1 {
				return e.inline.run(fd, op, b, off)
			}
		} else {
			// Protect the TID check from goroutine migration. Unmatched
			// workers immediately unlock before waiting on the shared ring.
			runtime.LockOSThread()
			if int32(unix.Gettid()) == bound {
				defer runtime.UnlockOSThread()
				return e.inline.run(fd, op, b, off)
			}
			runtime.UnlockOSThread()
		}
	}
	q.fd, q.op, q.buffer, q.offset = fd, op, b, off
	e.incoming <- q
	e.wake()
	<-q.done
	runtime.KeepAlive(b)
	return q.result, q.err
}

func (e *executor) complete(q *request, result int, err error) {
	q.pin.Unpin()
	q.buffer = nil
	q.result, q.err = result, err
	q.done <- struct{}{}
}

func (e *executor) completeGroup(group *requestGroup, result int, err error) int {
	count := group.count
	for i := 0; i < count; i++ {
		q := group.members[i]
		n := min(result, len(q.buffer))
		result -= n
		group.members[i] = nil
		e.complete(q, n, err)
	}
	group.count = 0
	return count
}

func (e *executor) poll() {
	r := e.ring
	at := r.enqueueTag(e.event, 6, 0, 0, 0, 0) & r.sqMask
	little.PutUint32(r.sqes[at*64+28:], unix.POLLIN)
}

func (e *executor) loop() {
	defer close(e.done)
	r := e.ring
	var pending [maxRings + 1]requestGroup
	var failure error
	active := 0
	e.poll()
	for {
		dataCompleted := false
		// Only this goroutine modifies the ring and request pin state.
		head := atomic.LoadUint32(r.cqHead)
		for head != atomic.LoadUint32(r.cqTail) {
			at := r.cqOffset + (head&r.cqMask)*16
			tag := little.Uint64(r.cq[at:])
			result := int32(little.Uint32(r.cq[at+8:]))
			head++
			atomic.StoreUint32(r.cqHead, head)
			if tag == 0 {
				var b [8]byte
				for {
					_, err := unix.Read(e.event, b[:])
					if err == unix.EINTR {
						continue
					}
					break
				}
				e.notified.Store(false)
				if failure == nil && !e.stopping.Load() {
					e.poll()
				}
			} else if tag <= maxRings && pending[tag].count != 0 {
				var err error
				if result < 0 {
					err = unix.Errno(-result)
					result = 0
				}
				active -= e.completeGroup(&pending[tag], int(result), errors.Join(failure, err))
				dataCompleted = true
			} else {
				panic("fileio: unexpected io_uring completion tag")
			}
		}
		if dataCompleted {
			// Let completed callers and their queue refill incoming before
			// deciding whether this batch needs another enter/wakeup cycle.
			runtime.Gosched()
		}
		var staged [maxRings]*request
		count := 0
		if active == 0 && len(e.incoming) == 0 {
			select {
			case q := <-e.incoming:
				staged[count] = q
				count++
				if e.clients.Load() > 1 {
					runtime.Gosched()
				}
			case <-e.stop:
				return
			}
		}
	drain:
		for count < len(staged) {
			select {
			case q := <-e.incoming:
				staged[count] = q
				count++
			default:
				break drain
			}
		}
		batch := staged[:count]
		enqueue := func(members []*request) {
			e.requests += uint64(len(members))
			if failure != nil {
				for _, q := range members {
					e.complete(q, 0, failure)
				}
				return
			}
			first := members[0]
			group := &pending[first.id]
			if group.count != 0 {
				panic("fileio: active group reused")
			}
			group.count = len(members)
			vectors := e.iovecs[int(first.id)*vectorBytes:]
			var address uint64
			for i, q := range members {
				group.members[i] = q
				address = 0
				if len(q.buffer) != 0 {
					q.pin.Pin(&q.buffer[0])
					address = uint64(uintptr(unsafe.Pointer(&q.buffer[0])))
				}
				little.PutUint64(vectors[i*16:], address)
				little.PutUint64(vectors[i*16+8:], uint64(len(q.buffer)))
			}
			active += len(members)
			if len(members) == 1 {
				r.enqueueTag(first.fd, first.op, address, len(first.buffer), first.offset, first.id)
			} else {
				e.coalescedWrites++
				e.coalescedRequests += uint64(len(members))
				r.enqueueTag(first.fd, opWritev, uint64(uintptr(unsafe.Pointer(&vectors[0]))), len(members), first.offset, first.id)
			}
		}
		for start := 0; start < len(batch); {
			if !alignedWrite(batch[start]) {
				enqueue(batch[start : start+1])
				start++
				continue
			}
			end := start + 1
			for end < len(batch) && alignedWrite(batch[end]) {
				end++
			}
			writes := batch[start:end]
			// Do not reorder across reads, fsync or unaligned operations. Only
			// strictly adjoining ranges enter one vector; overlaps stay separate.
			slices.SortFunc(writes, func(a, b *request) int {
				if order := cmp.Compare(a.fd, b.fd); order != 0 {
					return order
				}
				return cmp.Compare(a.offset, b.offset)
			})
			for i := 0; i < len(writes); {
				j := writeGroupEnd(writes, i)
				enqueue(writes[i:j])
				i = j
			}
			start = end
		}
		if e.stopping.Load() && active == 0 {
			return
		}
		if failure != nil && active == 0 {
			continue
		}
		e.sleeping.Store(true)
		// Requests arriving before sleeping became visible are found here; later
		// arrivals write eventfd, which wakes the outstanding poll operation.
		if (e.stopping.Load() && active == 0) || len(e.incoming) != 0 || atomic.LoadUint32(r.cqHead) != atomic.LoadUint32(r.cqTail) {
			e.sleeping.Store(false)
			continue
		}
		countPending := atomic.LoadUint32(r.sqTail) - atomic.LoadUint32(r.sqHead)
		e.enters++
		e.submissions += uint64(countPending)
		errno := e.enter(r, countPending)
		e.sleeping.Store(false)
		if errno == 0 || errno == unix.EINTR || errno == unix.EAGAIN || errno == unix.EBUSY {
			continue
		}
		if failure == nil {
			failure = fmt.Errorf("io_uring_enter: %w", errno)
			e.broken.Store(&executorError{failure})
			// With no SQPOLL, enter has stopped consuming. Withdraw only SQEs that
			// were not consumed, before releasing their referenced Go buffers.
			head, tail := atomic.LoadUint32(r.sqHead), atomic.LoadUint32(r.sqTail)
			for pos := head; pos != tail; pos++ {
				at := pos & r.sqMask
				tag := little.Uint64(r.sqes[at*64+32:])
				if tag != 0 && pending[tag].count != 0 {
					active -= e.completeGroup(&pending[tag], 0, failure)
				}
			}
			atomic.StoreUint32(r.sqTail, head)
			// Cancel consumed operations synchronously, then still drain every CQE.
			// Older kernels may reject this register operation; pins remain until CQE.
			var cancel [64]byte
			little.PutUint32(cancel[8:], ^uint32(0))
			little.PutUint32(cancel[12:], 5)
			little.PutUint64(cancel[16:], ^uint64(0))
			little.PutUint64(cancel[24:], ^uint64(0))
			for {
				_, _, x := unix.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(r.fd), 24, uintptr(unsafe.Pointer(&cancel[0])), 1, 0, 0)
				if x != unix.EINTR {
					break
				}
			}
		}

		_ = unix.Nanosleep(&unix.Timespec{Nsec: 1_000_000}, nil)
	}
}

func (e *executor) close() error {
	e.stopping.Store(true)
	close(e.stop)
	e.wake()
	<-e.done
	return errors.Join(e.ring.close(), e.inline.close(), unix.Munmap(e.iovecs), unix.Close(e.event))
}
