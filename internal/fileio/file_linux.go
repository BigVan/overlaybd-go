//go:build linux && (amd64 || arm64)

// Package fileio provides synchronous file methods backed by Linux io_uring.
// The data path uses IORING_OP_READ, IORING_OP_WRITE and IORING_OP_FSYNC.
package fileio

import (
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	alignment   = 4096
	maxTransfer = 1 << 20
	maxRings    = 64
	stripeCount = 4096
)

// File owns its descriptor and a lazily grown ring pool. Concurrent operations
// borrow separate read rings and reusable write request slots. Buffered writes
// use an adaptive shared executor; buffered reads remain in the calling goroutine.
// Close, Sync and Truncate wait for all current calls.
// Ordinary writes do not sync. In direct mode, unaligned writes preserve the
// rest of every physical 4 KiB block and may pad the physical EOF with zeros to
// a 4 KiB boundary. Stat and ReadAt observe that physical file size. Padding is
// never truncated implicitly, because that could discard concurrent appends.
// The caller must not close the descriptor returned by Fd independently.
type File struct {
	life     sync.RWMutex
	closed   bool
	file     *os.File
	fd       int
	name     string
	direct   bool
	executor *executor
	writer   *executor
	poolMu   sync.Mutex
	all      []*ring
	idle     chan *ring
	stripes  [stripeCount]sync.RWMutex
}

// Open opens path with flags and, when direct is true, O_DIRECT. New files use
// mode 0600. O_APPEND is rejected: positional writes must honor their offsets.
func Open(path string, flags int, direct bool) (*File, error) {
	if flags&os.O_APPEND != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: unix.EINVAL}
	}
	flags &^= unix.O_DIRECT
	if direct {
		flags |= unix.O_DIRECT
	}
	raw, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	var r *ring
	var executor, writer *executor
	if direct {
		executor, err = newExecutor()
		if err == nil {
			r = executor.client(1)
		}
	} else {
		r, err = openRing()
		if err == nil && flags&(os.O_WRONLY|os.O_RDWR) != 0 {
			writer, err = newExecutor()
			if err != nil {
				_ = r.close()
			} else {
				r.writerRequest = writer.client(1).request
			}
		}
	}
	if err != nil {
		_ = raw.Close()
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := &File{file: raw, fd: int(raw.Fd()), name: path, direct: direct, idle: make(chan *ring, maxRings), all: []*ring{r}, executor: executor, writer: writer}
	f.idle <- r
	return f, nil
}

func (f *File) borrow() (*ring, error) {
	select {
	case r := <-f.idle:
		return r, nil
	default:
	}
	f.poolMu.Lock()
	if len(f.all) < maxRings {
		var r *ring
		var err error
		if f.executor != nil {
			r = f.executor.client(uint64(len(f.all) + 1))
		} else {
			r, err = openRing()
			if err == nil && f.writer != nil {
				r.writerRequest = f.writer.client(uint64(len(f.all) + 1)).request
			}
		}
		if err == nil {
			f.all = append(f.all, r)
		}
		f.poolMu.Unlock()
		return r, err
	}
	f.poolMu.Unlock()
	return <-f.idle, nil
}

func (f *File) pathError(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	return &os.PathError{Op: op, Path: f.name, Err: err}
}

func (f *File) valid(off int64, n int) error {
	if f.closed {
		return os.ErrClosed
	}
	if off < 0 || int64(n) > math.MaxInt64-off {
		return unix.EINVAL
	}
	if f.direct && n > 0 && off+int64(n) > math.MaxInt64-(alignment-1) {
		return unix.EINVAL
	}
	return nil
}

func aligned(p []byte) bool { return len(p) == 0 || uintptr(unsafe.Pointer(&p[0]))&(alignment-1) == 0 }

// rangeGuard locks stripes in physical-block order, regardless of wraparound.
// All direct writes participate, including full aligned blocks; otherwise a
// concurrent aligned write could be lost by a neighboring sector's RMW.
type rangeGuard struct {
	f            *File
	first, count int
	write        bool
}

func (f *File) lockRange(off int64, n int, write bool) rangeGuard {
	first := off / alignment
	count := (off+int64(n)+alignment-1)/alignment - first
	g := rangeGuard{f: f, first: int(first % stripeCount), count: int(min(count, stripeCount)), write: write}
	if g.count == stripeCount {
		g.first = 0
	}
	g.each(func(m *sync.RWMutex) {
		if write {
			m.Lock()
		} else {
			m.RLock()
		}
	})
	return g
}
func (g rangeGuard) each(fn func(*sync.RWMutex)) {
	end := g.first + g.count
	if end > stripeCount {
		for i := 0; i < end-stripeCount; i++ {
			fn(&g.f.stripes[i])
		}
	}
	for i := g.first; i < min(end, stripeCount); i++ {
		fn(&g.f.stripes[i])
	}
}
func (g rangeGuard) unlock() {
	g.each(func(m *sync.RWMutex) {
		if g.write {
			m.Unlock()
		} else {
			m.RUnlock()
		}
	})
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	f.life.RLock()
	defer f.life.RUnlock()
	if err := f.valid(off, len(p)); err != nil {
		return 0, f.pathError("readat", err)
	}
	if len(p) == 0 {
		return 0, nil
	}
	r, err := f.borrow()
	if err != nil {
		return 0, f.pathError("readat", err)
	}
	defer func() { f.idle <- r }()
	n := 0
	for n < len(p) {
		length := min(len(p)-n, maxTransfer)
		var got int
		if f.direct {
			got, err = f.readDirect(r, p[n:n+length], off+int64(n))
		} else {
			got, err = r.run(f.fd, opRead, p[n:n+length], off+int64(n))
		}
		n += got
		if err != nil {
			return n, f.pathError("readat", err)
		}
		if got == 0 {
			return n, io.EOF
		}
	}
	return n, nil
}

func (f *File) readDirect(r *ring, p []byte, off int64) (int, error) {
	guard := f.lockRange(off, len(p), false)
	defer guard.unlock()
	if off%alignment == 0 && len(p)%alignment == 0 && aligned(p) {
		n, err := r.run(f.fd, opRead, p, off)
		if err == nil && n < len(p) {
			err = io.EOF
		}
		return n, err
	}
	start := off &^ (int64(alignment) - 1)
	prefix := int(off - start)
	length := (prefix + len(p) + alignment - 1) &^ (alignment - 1)
	buf, err := r.buffer(length)
	if err != nil {
		return 0, err
	}
	n, err := r.run(f.fd, opRead, buf, start)
	got := min(len(p), max(0, n-prefix))
	copy(p[:got], buf[prefix:prefix+got])
	if err == nil && got < len(p) {
		err = io.EOF
	}
	return got, err
}

func (f *File) WriteAt(p []byte, off int64) (int, error) {
	f.life.RLock()
	defer f.life.RUnlock()
	if err := f.valid(off, len(p)); err != nil {
		return 0, f.pathError("writeat", err)
	}
	if len(p) == 0 {
		return 0, nil
	}
	r, err := f.borrow()
	if err != nil {
		return 0, f.pathError("writeat", err)
	}
	defer func() { f.idle <- r }()
	n := 0
	for n < len(p) {
		length := min(len(p)-n, maxTransfer)
		var wrote int
		if f.direct {
			wrote, err = f.writeDirect(r, p[n:n+length], off+int64(n))
		} else if f.writer != nil {
			wrote, err = f.writer.run(r.writerRequest, f.fd, opWrite, p[n:n+length], off+int64(n))
		} else {
			wrote, err = r.run(f.fd, opWrite, p[n:n+length], off+int64(n))
		}
		n += wrote
		if err != nil {
			return n, f.pathError("writeat", err)
		}
		if wrote == 0 {
			return n, f.pathError("writeat", io.ErrShortWrite)
		}
	}
	return n, nil
}

func (f *File) writeDirect(r *ring, p []byte, off int64) (int, error) {
	guard := f.lockRange(off, len(p), true)
	defer guard.unlock()
	if off%alignment == 0 && len(p)%alignment == 0 && aligned(p) {
		return r.run(f.fd, opWrite, p, off)
	}
	start := off &^ (int64(alignment) - 1)
	prefix := int(off - start)
	length := (prefix + len(p) + alignment - 1) &^ (alignment - 1)
	buf, err := r.buffer(length)
	if err != nil {
		return 0, err
	}
	if prefix != 0 || len(p)%alignment != 0 {
		read, err := r.run(f.fd, opRead, buf, start)
		if err != nil {
			return 0, err
		}
		clear(buf[read:])
	}
	copy(buf[prefix:prefix+len(p)], p)
	written := 0
	for written < len(buf) {
		n, err := r.run(f.fd, opWrite, buf[written:], start+int64(written))
		written += n
		if err != nil {
			return min(len(p), max(0, written-prefix)), err
		}
		if n == 0 || (n%alignment != 0 && written < len(buf)) {
			return min(len(p), max(0, written-prefix)), io.ErrShortWrite
		}
	}
	return len(p), nil
}

// BindIOThread drains active file calls and marks the current OS thread as
// the inline I/O owner. The queue must keep its goroutine locked to that thread
// while serving requests. The hint does not itself retain an OS thread lock.
// Unbound standalone files retain their adaptive execution policy.
func (f *File) BindIOThread() error {
	f.life.Lock()
	defer f.life.Unlock()
	if f.closed {
		return f.pathError("bindiothread", os.ErrClosed)
	}
	tid := int32(unix.Gettid())
	if f.executor != nil {
		f.executor.boundThread.Store(tid)
	}
	if f.writer != nil {
		f.writer.boundThread.Store(tid)
	}
	return nil
}

// Sync is a barrier for in-flight operations and submits a real uring FSYNC.
func (f *File) Sync() error {
	f.life.Lock()
	defer f.life.Unlock()
	if f.closed {
		return f.pathError("sync", os.ErrClosed)
	}
	r, err := f.borrow()
	if err != nil {
		return f.pathError("sync", err)
	}
	defer func() { f.idle <- r }()
	if f.writer != nil {
		_, err = f.writer.run(r.writerRequest, f.fd, opFsync, nil, 0)
	} else {
		_, err = r.run(f.fd, opFsync, nil, 0)
	}
	return f.pathError("sync", err)
}
func (f *File) Stat() (os.FileInfo, error) {
	f.life.RLock()
	defer f.life.RUnlock()
	if f.closed {
		return nil, f.pathError("stat", os.ErrClosed)
	}
	return f.file.Stat()
}
func (f *File) Truncate(size int64) error {
	f.life.Lock()
	defer f.life.Unlock()
	if f.closed {
		return f.pathError("truncate", os.ErrClosed)
	}
	return f.file.Truncate(size)
}
func (f *File) Name() string { return f.name }
func (f *File) Fd() uintptr {
	f.life.RLock()
	defer f.life.RUnlock()
	if f.closed {
		return ^uintptr(0)
	}
	return uintptr(f.fd)
}

// Close drains current callers before releasing rings, scratch buffers and the
// file descriptor. Like os.File.Close, it does not itself impose durability.
func (f *File) Close() error {
	f.life.Lock()
	defer f.life.Unlock()
	if f.closed {
		return f.pathError("close", os.ErrClosed)
	}
	f.closed = true
	var err error
	if f.executor != nil {
		err = f.executor.close()
	}
	if f.writer != nil {
		err = errors.Join(err, f.writer.close())
	}
	for _, r := range f.all {
		err = errors.Join(err, r.close())
	}
	return errors.Join(err, f.file.Close())
}
