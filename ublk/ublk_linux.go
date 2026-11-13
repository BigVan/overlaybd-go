//go:build linux && (amd64 || arm64)

// Package ublk exposes a Go block store as a Linux ublk device using io_uring.
package ublk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const maxIO = 128 * 1024

type Backend interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	ReadOnly() bool
	Zero(offset, length int64) error
	Sync() error
}

type Options struct {
	ID         int // -1 asks the kernel to allocate an unused device ID.
	QueueDepth int // 0 defaults to 32. One hardware queue is used.
	// Concurrent allows up to QueueDepth backend calls in flight. The backend
	// must support concurrent I/O. Flush, FUA, discard and zeroes form barriers.
	Concurrent bool
	// InlineReads runs ordinary reads on the queue thread in Concurrent mode.
	// Reads may overlap writes; flush, FUA and metadata barriers still drain.
	InlineReads bool
	// InlineSingle avoids worker handoffs when an otherwise idle queue has only
	// one ready request. Use only when backend calls progress independently.
	InlineSingle bool
}
type Info struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	ReadOnly bool   `json:"readOnly"`
}

type control struct {
	file   *os.File
	ring   *ring
	buffer []byte
}

func openControl() (*control, error) {
	f, err := os.OpenFile("/dev/ublk-control", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ublk-control (load ublk_drv and run with device permissions): %w", err)
	}
	r, err := newRing(4, 0)
	if err != nil {
		f.Close()
		return nil, err
	}
	b, err := anon(4096)
	if err != nil {
		r.close()
		f.Close()
		return nil, err
	}
	return &control{file: f, ring: r, buffer: b}, nil
}
func (c *control) close() { c.ring.close(); unix.Munmap(c.buffer); c.file.Close() }
func (c *control) command(op uint32, id int, payload []byte, data uint64) error {
	var cmd [32]byte
	le.PutUint32(cmd[:], uint32(id))
	le.PutUint16(cmd[4:], 0xffff)
	if len(payload) > 0 {
		copy(c.buffer, payload)
		le.PutUint16(cmd[6:], uint16(len(payload)))
		le.PutUint64(cmd[8:], address(c.buffer))
	}
	le.PutUint64(cmd[16:], data)
	if err := c.ring.enqueue(int(c.file.Fd()), 0xc0207500|op, cmd[:], 0); err != nil {
		return err
	}
	if err := c.ring.enter(false); err != nil {
		return err
	}
	_, res, err := c.ring.completion()
	if err != nil {
		return err
	}
	if res < 0 {
		return fmt.Errorf("ublk control command %#x: %w", op, unix.Errno(-res))
	}
	copy(payload, c.buffer[:len(payload)])
	return nil
}

// Serve creates and starts a device and serves until cancellation or an I/O
// queue failure. ready runs after START_DEV succeeds. The caller owns backend.
// SIGINT/SIGTERM should cancel ctx. Serve stops/deletes its device before return.
func Serve(ctx context.Context, backend Backend, opt Options, ready func(Info)) (err error) {
	if opt.QueueDepth == 0 {
		opt.QueueDepth = 32
	}
	if opt.ID < -1 || opt.ID > 0x7fffffff || opt.QueueDepth < 1 || opt.QueueDepth > 128 {
		return errors.New("ublk: id must be -1 or nonnegative; queue depth must be 1..128")
	}
	if backend.Size() <= 0 || backend.Size()%512 != 0 {
		return errors.New("ublk: size must be a positive multiple of 512")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	c, err := openControl()
	if err != nil {
		return err
	}
	defer c.close()
	info := make([]byte, 64)
	le.PutUint16(info, 1)
	le.PutUint16(info[2:], uint16(opt.QueueDepth))
	le.PutUint32(info[8:], maxIO)
	le.PutUint32(info[12:], uint32(opt.ID))
	le.PutUint64(info[24:], 1<<6)
	if err = c.command(4, opt.ID, info, 0); err != nil {
		return fmt.Errorf("add device (Linux 6.6+ required): %w", err)
	}
	id := int(le.Uint32(info[12:]))
	defer func() { err = errors.Join(err, ignoreGone(c.command(5, id, nil, 0))) }()
	params := make([]byte, 80)
	le.PutUint32(params, 80)
	le.PutUint32(params[4:], 1)
	attrs := uint32(4 | 8) // volatile write cache, FUA supported through Sync.
	if backend.ReadOnly() {
		attrs = 1
	} else {
		le.PutUint32(params[4:], 3)
		le.PutUint32(params[44:], 512)
		le.PutUint32(params[48:], maxIO/512)
		le.PutUint32(params[52:], maxIO/512)
		le.PutUint16(params[56:], 1)
	}
	le.PutUint32(params[8:], attrs)
	params[12], params[13], params[14], params[15] = 9, 12, 12, 9
	le.PutUint32(params[16:], maxIO/512)
	le.PutUint64(params[24:], uint64(backend.Size()/512))
	if err = c.command(8, id, params, 0); err != nil {
		return err
	}
	prepared := make(chan error, 1)
	done := make(chan error, 1)
	release := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(exited)
		q, e := newQueue(id, opt.QueueDepth, backend, opt.Concurrent)
		if e != nil {
			prepared <- e
			done <- e
			return
		}
		defer q.close()
		q.inlineReads = opt.InlineReads
		q.inlineSingle = opt.InlineSingle
		e = bindBackendIOThread(backend)
		if e == nil {
			e = q.prime()
		}
		prepared <- e
		if e == nil {
			if opt.Concurrent {
				e = q.runConcurrent()
			} else {
				e = q.run()
			}
		}
		done <- e
		// Keep mappings alive until STOP_DEV has canceled kernel access.
		<-release
	}()
	defer func() {
		err = errors.Join(err, ignoreGone(c.command(7, id, nil, 0)))
		close(release)
		<-exited
		err = errors.Join(err, backend.Sync())
	}()
	if err = <-prepared; err != nil {
		return err
	}
	if err = c.command(6, id, nil, uint64(os.Getpid())); err != nil {
		return err
	}
	if ready != nil {
		ready(Info{ID: id, Path: fmt.Sprintf("/dev/ublkb%d", id), Size: backend.Size(), ReadOnly: backend.ReadOnly()})
	}
	select {
	case <-ctx.Done():
		return nil
	case err = <-done:
		return err
	}
}

// Called on the locked queue thread, before any requests are submitted.
func bindBackendIOThread(backend Backend) error {
	if binder, ok := backend.(interface{ BindIOThread() error }); ok {
		return binder.BindIOThread()
	}
	return nil
}

func ignoreGone(err error) error {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODEV) {
		return nil
	}
	return err
}

// Delete stops and removes the explicitly selected device.
func Delete(id int) error {
	if id < 0 || id > 0x7fffffff {
		return errors.New("ublk: invalid device id")
	}
	c, err := openControl()
	if err != nil {
		return err
	}
	defer c.close()
	if err = c.command(7, id, nil, 0); err != nil {
		return err
	}
	return ignoreGone(c.command(5, id, nil, 0))
}

type queue struct {
	file          *os.File
	ring          *ring
	desc, buffers []byte
	depth         int
	backend       Backend
	inlineReads   bool
	inlineSingle  bool
}

func newQueue(id, depth int, b Backend, concurrent bool) (q *queue, err error) {
	q = &queue{depth: depth, backend: b}
	defer func() {
		if err != nil {
			q.close()
			q = nil
		}
	}()
	// devtmpfs/udev can publish the character device just after ADD_DEV returns.
	for attempt := 0; attempt < 100; attempt++ {
		q.file, err = os.OpenFile(fmt.Sprintf("/dev/ublkc%d", id), os.O_RDWR, 0)
		if !errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return q, err
	}
	// Like the upstream queue, avoid forcing an IPI for every completion.
	// The locked queue thread runs pending task work at its next kernel entry.
	// COOP_TASKRUN predates the Linux 6.6 minimum required by our command ABI.
	entries := depth
	if concurrent {
		entries++
	} // one additional eventfd poll command
	q.ring, err = newRing(uint32(entries), setupCoopTaskrun)
	if err != nil {
		return q, err
	}
	page := os.Getpagesize()
	length := (depth*24 + page - 1) / page * page
	q.desc, err = unix.Mmap(int(q.file.Fd()), 0, length, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return q, err
	}
	q.buffers, err = anon(depth * maxIO)
	return q, err
}
func (q *queue) close() {
	if q.ring != nil {
		q.ring.close()
	}
	if q.buffers != nil {
		unix.Munmap(q.buffers)
	}
	if q.desc != nil {
		unix.Munmap(q.desc)
	}
	if q.file != nil {
		q.file.Close()
	}
}
func (q *queue) submit(tag int, result int32, fetch bool) error {
	var cmd [16]byte
	le.PutUint16(cmd[2:], uint16(tag))
	le.PutUint32(cmd[4:], uint32(result))
	le.PutUint64(cmd[8:], address(q.buffers[tag*maxIO:]))
	op := uint32(0xc0107521)
	if fetch {
		op = 0xc0107520
	}
	return q.ring.enqueue(int(q.file.Fd()), op, cmd[:], uint64(tag))
}
func (q *queue) prime() error {
	for tag := 0; tag < q.depth; tag++ {
		if err := q.submit(tag, 0, true); err != nil {
			return err
		}
	}
	return q.ring.enter(false)
}
func (q *queue) run() error {
	remaining := q.depth
	for remaining > 0 {
		tag, res, err := q.ring.completion()
		if err != nil {
			return err
		}
		if tag >= uint64(q.depth) {
			return errors.New("ublk: invalid completion tag")
		}
		if res == -int32(unix.ENODEV) || res == -int32(unix.ECANCELED) {
			remaining--
			continue
		}
		if res != 0 {
			return fmt.Errorf("ublk fetch: result %d", res)
		}
		desc := q.desc[tag*24 : (tag+1)*24]
		result := handle(q.backend, le.Uint32(desc), le.Uint64(desc[8:]), le.Uint32(desc[4:]), q.buffers[int(tag)*maxIO:(int(tag)+1)*maxIO])
		if err = q.submit(int(tag), result, false); err != nil {
			return err
		}
		// Batch ready completions without holding a full queue's results. Submit
		// immediately when the CQ is drained, retaining QD1 wakeup behavior.
		// Backend I/O and flush/FUA keep their original completion order.
		if q.ring.pending() >= 4 || !q.ring.ready() {
			if err = q.ring.enter(false); err != nil {
				return err
			}
		}
	}
	return nil
}

// dispatchInline selects the execution path after barrier requests have been
// handled. It does not consume CQEs or alter completion batching.
func (q *queue) dispatchInline(op uint32, inflight int, wakeTag uint64) bool {
	return (q.inlineReads && op == 0) || (q.inlineSingle && inflight == 0 && !q.ring.readyExcept(wakeTag))
}

// runConcurrent keeps ring ownership on the queue thread and gives each tag's
// buffer to at most one worker. An eventfd poll wakes the queue for backend
// results while allowing newly arriving fetches to progress at the same time.
func (q *queue) runConcurrent() error {
	type request struct {
		tag     int
		flags   uint32
		sector  uint64
		sectors uint32
	}
	type completion struct {
		tag    int
		result int32
	}
	jobs := make(chan request, q.depth)
	results := make(chan completion, q.depth)
	wakeFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(wakeFD)
	const wakeTag = ^uint64(0)
	if err := q.ring.enqueuePoll(wakeFD, wakeTag); err != nil {
		return err
	}
	if err := q.ring.enter(false); err != nil {
		return err
	}
	var wakePending atomic.Bool
	var wakeValue [8]byte
	le.PutUint64(wakeValue[:], 1)
	var workers sync.WaitGroup
	for i := 0; i < q.depth; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for req := range jobs {
				buf := q.buffers[req.tag*maxIO : (req.tag+1)*maxIO]
				results <- completion{req.tag, handle(q.backend, req.flags, req.sector, req.sectors, buf)}
				// Coalesce notifications until the queue consumes the poll CQE.
				if wakePending.CompareAndSwap(false, true) {
					for {
						_, err := unix.Write(wakeFD, wakeValue[:])
						if err != unix.EINTR {
							break
						}
					}
				}
			}
		}()
	}
	// Even on a ring failure, workers must release their buffers before close
	// unmaps them. Capacity depth lets every result finish without a consumer.
	defer func() { close(jobs); workers.Wait() }()
	inflight := 0
	commit := func(c completion) error {
		inflight--
		if err := q.submit(c.tag, c.result, false); err != nil {
			return err
		}
		if q.ring.pending() >= 4 || !q.ring.ready() {
			return q.ring.enter(false)
		}
		return nil
	}
	remaining := q.depth
	for remaining > 0 {
		// Return available results promptly, including when fetch completions
		// remain ready. Each commit also fetches the next request for that tag.
		for inflight > 0 {
			select {
			case c := <-results:
				if err := commit(c); err != nil {
					return err
				}
			default:
				goto fetched
			}
		}
	fetched:
		if q.ring.pending() > 0 && !q.ring.ready() {
			if err := q.ring.enter(false); err != nil {
				return err
			}
		}
		tag, res, err := q.ring.completion()
		if err != nil {
			return err
		}
		if tag == wakeTag {
			if res < 0 {
				return fmt.Errorf("ublk wake poll: %w", unix.Errno(-res))
			}
			var value [8]byte
			for {
				_, err := unix.Read(wakeFD, value[:])
				if err == unix.EINTR {
					continue
				}
				if err != nil {
					return fmt.Errorf("ublk wake read: %w", err)
				}
				break
			}
			// Clear before draining results: a worker publishing after the
			// next drain must generate a fresh notification.
			wakePending.Store(false)
			if err := q.ring.enqueuePoll(wakeFD, wakeTag); err != nil {
				return err
			}
			continue
		}
		if tag >= uint64(q.depth) {
			return errors.New("ublk: invalid completion tag")
		}
		if res == -int32(unix.ENODEV) || res == -int32(unix.ECANCELED) {
			remaining--
			continue
		}
		if res != 0 {
			return fmt.Errorf("ublk fetch: result %d", res)
		}
		desc := q.desc[tag*24 : (tag+1)*24]
		req := request{int(tag), le.Uint32(desc), le.Uint64(desc[8:]), le.Uint32(desc[4:])}
		op := req.flags & 255
		if (op != 0 && op != 1) || req.flags&(1<<13) != 0 {
			// Do not let any request cross a flush/FUA or metadata operation.
			for inflight > 0 {
				if err := commit(<-results); err != nil {
					return err
				}
			}
			buf := q.buffers[req.tag*maxIO : (req.tag+1)*maxIO]
			result := handle(q.backend, req.flags, req.sector, req.sectors, buf)
			if err := q.submit(req.tag, result, false); err != nil {
				return err
			}
			if err := q.ring.enter(false); err != nil {
				return err
			}
		} else if q.dispatchInline(op, inflight, wakeTag) {
			// Reads can keep the cache-hit path while unrelated writes are in
			// flight. InlineSingle also avoids handoffs for an idle queue's sole
			// request. Only the barrier branch above drains backend workers.
			buf := q.buffers[req.tag*maxIO : (req.tag+1)*maxIO]
			result := handle(q.backend, req.flags, req.sector, req.sectors, buf)
			if err := q.submit(req.tag, result, false); err != nil {
				return err
			}
			if q.ring.pending() >= 4 || !q.ring.ready() {
				if err := q.ring.enter(false); err != nil {
					return err
				}
			}
		} else {
			inflight++
			jobs <- req
		}
	}
	return nil
}

func handle(b Backend, opFlags uint32, sector uint64, sectors uint32, buf []byte) int32 {
	op := opFlags & 255
	if op == 2 {
		if err := b.Sync(); err != nil {
			return -int32(unix.EIO)
		}
		return 0
	}
	if op != 0 && op != 1 && op != 3 && op != 5 {
		return -int32(unix.EOPNOTSUPP)
	}
	if sector > uint64(b.Size()/512) || uint64(sectors) > uint64(b.Size()/512)-sector || uint64(sectors)*512 > uint64(len(buf)) {
		return -int32(unix.EINVAL)
	}
	if op != 0 && b.ReadOnly() {
		return -int32(unix.EROFS)
	}
	p := buf[:int(sectors)*512]
	off := int64(sector * 512)
	var n int
	var err error
	switch op {
	case 0:
		n, err = b.ReadAt(p, off)
	case 1:
		n, err = b.WriteAt(p, off)
	case 3, 5:
		err = b.Zero(off, int64(len(p)))
	}
	if err != nil || ((op == 0 || op == 1) && n != len(p)) {
		return -int32(unix.EIO)
	}
	if op != 0 && opFlags&(1<<13) != 0 {
		if err = b.Sync(); err != nil {
			return -int32(unix.EIO)
		}
	}
	return int32(n)
}
