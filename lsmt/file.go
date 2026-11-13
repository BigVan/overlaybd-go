package lsmt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/bigvan/overlaybd-go/internal/fileio"
	"github.com/bigvan/overlaybd-go/zfile"
	"golang.org/x/sys/unix"
)

// IOEngine selects the writable data file backend. Index files always use
// buffered I/O. Values match OverlayBD's ioEngine configuration.
type IOEngine int

const (
	PSync           IOEngine = 0
	IOUringBuffered IOEngine = 3
	IOUringDirect   IOEngine = 4
)

type OpenOptions struct {
	IOEngine IOEngine
}

type dataFile interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Close() error
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Fd() uintptr
	Name() string
}

// File owns its underlying files. Writes must be sector aligned; reads may be
// byte aligned. Only one writable handle may open a data/index pair at a time.
type File struct {
	// Lock order: mu (lifecycle/flush barrier), hybrid ranges, logMu, indexMu. I/O
	// holds mu for reading, so data writes do not serialize with each other.
	mu         sync.RWMutex
	logMu      sync.Mutex // indexEnd, writeErr, log append/publication order
	indexMu    sync.RWMutex
	ranges     rangeLocks // overlapping hybrid payload reads/writes
	h          header
	data       io.ReaderAt
	dataFile   dataFile
	indexFile  *os.File
	direct     bool
	compressed *zfile.Reader
	mappings   mappingIndex
	dataEnd    atomic.Int64
	indexEnd   int64
	closed     bool
	writeErr   error
}

func Create(dataPath, indexPath string, size int64) (*File, error) {
	return create(dataPath, indexPath, size, false)
}

// CreateHybrid creates an upstream-compatible layer that overwrites mapped
// payloads in place and appends only holes or explicitly zeroed ranges.
func CreateHybrid(dataPath, indexPath string, size int64) (*File, error) {
	return create(dataPath, indexPath, size, true)
}

func create(dataPath, indexPath string, size int64, hybrid bool) (*File, error) {
	if size <= 0 || size%SectorSize != 0 || uint64(size/SectorSize) >= invalidOffset {
		return nil, errors.New("lsmt: size must be a positive multiple of 512")
	}
	h, err := newHeader(size)
	if err != nil {
		return nil, err
	}
	var mode uint32
	if hybrid {
		mode = flagHybrid
		h.flags = mode
	}
	d, err := os.OpenFile(dataPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	i, err := os.OpenFile(indexPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		d.Close()
		os.Remove(dataPath)
		return nil, err
	}
	f := &File{h: h, data: d, dataFile: d, indexFile: i, indexEnd: headerSize}
	f.dataEnd.Store(headerSize)
	success := false
	defer func() {
		if !success {
			d.Close()
			i.Close()
			os.Remove(dataPath)
			os.Remove(indexPath)
		}
	}()
	if err = lockFiles(d, i); err != nil {
		return nil, err
	}
	h.flags = 3 | mode
	if err = writeAt(d, h.encode(), 0); err != nil {
		return nil, err
	}
	h.flags = 1 | mode
	h.index = headerSize
	if err = writeAt(i, h.encode(), 0); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	success = true
	return f, nil
}

func lockFiles(files ...dataFile) error {
	for _, f := range files {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return fmt.Errorf("lsmt: lock %s: %w", f.Name(), err)
		}
	}
	return nil
}

func OpenRW(dataPath, indexPath string) (*File, error) {
	return OpenRWWithOptions(dataPath, indexPath, OpenOptions{})
}

// OpenRWWithOptions selects the data backend without changing the on-disk
// format. Read-only lower layers and index logs retain their buffered backend.
func OpenRWWithOptions(dataPath, indexPath string, options OpenOptions) (*File, error) {
	var d dataFile
	var err error
	switch options.IOEngine {
	case PSync:
		d, err = os.OpenFile(dataPath, os.O_RDWR, 0)
	case IOUringBuffered, IOUringDirect:
		d, err = fileio.Open(dataPath, os.O_RDWR, options.IOEngine == IOUringDirect)
	default:
		return nil, fmt.Errorf("lsmt: unsupported ioEngine %d (use 0, 3 or 4)", options.IOEngine)
	}
	if err != nil {
		return nil, err
	}
	i, err := os.OpenFile(indexPath, os.O_RDWR, 0)
	if err != nil {
		d.Close()
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			d.Close()
			i.Close()
		}
	}()
	if err = lockFiles(d, i); err != nil {
		return nil, err
	}
	dh, err := readHeader(d, 0)
	if err != nil {
		return nil, err
	}
	ih, err := readHeader(i, 0)
	if err != nil {
		return nil, err
	}
	mode := dh.flags & flagHybrid
	if dh.flags != 3|mode || ih.flags != 1|mode || ih.index != headerSize || dh.uuid != ih.uuid || dh.size != ih.size || dh.parent != ih.parent {
		return nil, errors.New("lsmt: mismatched or non-writable data/index pair")
	}
	ds, err := d.Stat()
	if err != nil {
		return nil, err
	}
	is, err := i.Stat()
	if err != nil {
		return nil, err
	}
	dataEnd := ds.Size() / SectorSize * SectorSize
	indexEnd := headerSize + (is.Size()-headerSize)/16*16
	index, err := loadIndex(i, headerSize, uint64((indexEnd-headerSize)/16), int64(dh.size), dataEnd, false)
	if err != nil {
		return nil, err
	}
	// An interrupted append can leave an incomplete last record or unused data.
	// Validate every complete record before trimming either tail.
	if ds.Size() != dataEnd {
		if err = d.Truncate(dataEnd); err != nil {
			return nil, err
		}
	}
	if is.Size() != indexEnd {
		if err = i.Truncate(indexEnd); err != nil {
			return nil, err
		}
	}
	success = true
	f := &File{h: dh, data: d, dataFile: d, indexFile: i, mappings: index, indexEnd: indexEnd, direct: options.IOEngine == IOUringDirect}
	f.dataEnd.Store(dataEnd)
	return f, nil
}

// OpenRO opens a committed LSMT layer, automatically decoding a ZFile wrapper.
func OpenRO(path string) (*File, error) {
	d, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	f := &File{data: d, dataFile: d}
	success := false
	defer func() {
		if !success {
			if f.compressed != nil {
				f.compressed.Close()
			}
			d.Close()
		}
	}()
	st, err := d.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	var signature [8]byte
	if _, err = d.ReadAt(signature[:], 0); err != nil {
		return nil, err
	}
	if zfile.IsMagic(signature[:]) {
		f.compressed, err = zfile.NewReader(d, size)
		if err != nil {
			return nil, err
		}
		f.data = f.compressed
		size = f.compressed.Size()
	}
	if size < 2*headerSize {
		return nil, errors.New("lsmt: truncated sealed layer")
	}
	h, err := readHeader(f.data, 0)
	if err != nil {
		return nil, err
	}
	t, err := readHeader(f.data, size-headerSize)
	if err != nil {
		return nil, err
	}
	// close_seal() leaves the original unsealed data header in place.
	if h.flags&3 != 3 || t.flags & ^flagHybrid != 6 || h.uuid != t.uuid || h.size != t.size || h.parent != t.parent || t.index < headerSize || t.index%SectorSize != 0 || t.index > uint64(size-headerSize) || t.count > maxRecords || t.count*16 != uint64(size-headerSize)-t.index {
		return nil, errors.New("lsmt: invalid sealed layer metadata")
	}
	f.h = t
	f.mappings, err = loadIndex(f.data, t.index, t.count, int64(t.size), int64(t.index), true)
	if err != nil {
		return nil, err
	}
	success = true
	return f, nil
}

func (f *File) Size() int64    { return int64(f.h.size) }
func (f *File) ReadOnly() bool { return f.indexFile == nil }
func (f *File) Hybrid() bool   { return f.h.flags&flagHybrid != 0 }

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	n, err := readBounds(p, off, f.Size())
	if n == 0 {
		return n, err
	}
	clear(p[:n])
	if e := f.overlay(p[:n], off); e != nil {
		return 0, e
	}
	return n, err
}

func readBounds(p []byte, off, size int64) (int, error) {
	if off < 0 {
		return 0, errors.New("lsmt: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= size {
		return 0, io.EOF
	}
	if int64(len(p)) > size-off {
		return int(size - off), io.EOF
	}
	return len(p), nil
}

// overlay fills mapped ranges only, preserving lower-layer contents in holes.
func (f *File) overlay(p []byte, off int64) error {
	if f.Hybrid() && !f.ReadOnly() {
		guard := f.ranges.lock(off, int64(len(p)), false)
		defer guard.unlock()
	}
	f.indexMu.RLock()
	defer f.indexMu.RUnlock()
	end := off + int64(len(p))
	var readErr error
	f.mappings.visit(uint64(off)/SectorSize, (uint64(end)+SectorSize-1)/SectorSize, func(m mapping) bool {
		start := int64(m.offset * SectorSize)
		lo, hi := max(off, start), min(end, int64(m.end()*SectorSize))
		dst := p[lo-off : hi-off]
		if m.zero {
			clear(dst)
		} else {
			n, err := f.data.ReadAt(dst, int64(m.physical*SectorSize)+lo-start)
			if err != nil {
				readErr = err
				return false
			}
			if n != len(dst) {
				readErr = io.ErrUnexpectedEOF
				return false
			}
		}
		return true
	})
	return readErr
}

// WriteAt appends data/mappings except for hybrid in-place overwrites. Buffered
// backends write to the OS page cache; direct data I/O bypasses it. Call Sync for
// durability; ordinary writes do not impose persistence ordering.
func (f *File) WriteAt(p []byte, off int64) (int, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if err := f.writable(off, int64(len(p))); err != nil {
		return 0, err
	}
	if f.Hybrid() {
		guard := f.ranges.lock(off, int64(len(p)), true)
		defer guard.unlock()
		return f.writeHybrid(p, off)
	}
	return f.appendData(p, off)
}

func (f *File) appendData(p []byte, off int64) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxLength*SectorSize)
		// Reserve a unique append range without holding a mutex over data I/O.
		pos := f.reserveData(int64(n))
		m := mapping{offset: uint64(off / SectorSize), length: uint64(n / SectorSize), physical: uint64(pos / SectorSize)}
		if err := writeAt(f.dataFile, p[:n], pos); err != nil {
			f.logMu.Lock()
			f.writeErr = errors.Join(f.writeErr, err)
			f.logMu.Unlock()
			return written, err
		}
		if err := f.appendMapping(m); err != nil {
			return written, err
		}
		off += int64(n)
		p = p[n:]
		written += n
	}
	return written, nil
}

func (f *File) reserveData(n int64) int64 {
	if !f.direct {
		return f.dataEnd.Add(n) - n
	}
	// Direct payloads own complete physical blocks. This also handles reopening
	// a buffered layer whose existing tail is only sector aligned. Padding is
	// unreferenced by mappings and remains compatible with upstream readers.
	for {
		old := f.dataEnd.Load()
		pos := (old + 4095) &^ 4095
		end := pos + ((n + 4095) &^ 4095)
		if f.dataEnd.CompareAndSwap(old, end) {
			return pos
		}
	}
}

func (f *File) writable(off, length int64) error {
	if f.closed {
		return os.ErrClosed
	}
	if f.ReadOnly() {
		return errors.New("lsmt: read-only layer")
	}
	if off < 0 || length < 0 || off > f.Size() || length > f.Size()-off || off%SectorSize != 0 || length%SectorSize != 0 {
		return errors.New("lsmt: write must be sector aligned and within device bounds")
	}
	f.logMu.Lock()
	defer f.logMu.Unlock()
	if f.writeErr != nil {
		return fmt.Errorf("lsmt: previous write failed; reopen required: %w", f.writeErr)
	}
	return nil
}

func (f *File) appendMapping(m mapping) error {
	// Serialize the log append and publication together: overlapping writes
	// must have the same visible order in memory and when replaying the log.
	f.logMu.Lock()
	defer f.logMu.Unlock()
	if f.writeErr != nil {
		return fmt.Errorf("lsmt: previous write failed; reopen required: %w", f.writeErr)
	}
	if (f.indexEnd-headerSize)/16 >= maxRecords {
		return errors.New("lsmt: index full; commit to a new layer")
	}
	if err := writeAt(f.indexFile, m.encode(), f.indexEnd); err != nil {
		f.writeErr = errors.Join(err, f.indexFile.Truncate(f.indexEnd))
		return f.writeErr
	}
	f.indexEnd += 16
	f.indexMu.Lock()
	f.mappings.insert(m)
	f.indexMu.Unlock()
	return nil
}

// Zero records explicit zero extents, masking data in any lower layer.
func (f *File) Zero(off, length int64) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if err := f.writable(off, length); err != nil {
		return err
	}
	if f.Hybrid() {
		guard := f.ranges.lock(off, length, true)
		defer guard.unlock()
	}
	for length > 0 {
		n := min(length, int64(maxLength*SectorSize))
		// Zero extents have no payload. Do not reference a concurrent writer's
		// reserved offset, which may not yet exist in the data file.
		if err := f.appendMapping(mapping{offset: uint64(off / SectorSize), length: uint64(n / SectorSize), physical: headerSize / SectorSize, zero: true}); err != nil {
			return err
		}
		off += n
		length -= n
	}
	return nil
}

// sync requires exclusive mu ownership, excluding all in-flight writes.
func (f *File) sync() error {
	if f.ReadOnly() {
		return nil
	}
	if err := f.dataFile.Sync(); err != nil {
		return err
	}
	return errors.Join(f.writeErr, f.indexFile.Sync())
}

// BindIOThread lets an optional writable data backend specialize its I/O path
// for the calling OS thread. The caller must keep that thread locked.
func (f *File) BindIOThread() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return os.ErrClosed
	}
	if f.ReadOnly() {
		return nil
	}
	if binder, ok := f.dataFile.(interface{ BindIOThread() error }); ok {
		return binder.BindIOThread()
	}
	return nil
}

func (f *File) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return os.ErrClosed
	}
	return f.sync()
}
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	err := f.sync()
	f.closed = true
	if f.compressed != nil {
		err = errors.Join(err, f.compressed.Close())
	}
	err = errors.Join(err, f.dataFile.Close())
	if f.indexFile != nil {
		err = errors.Join(err, f.indexFile.Close())
	}
	return err
}

func writeAt(w io.WriterAt, b []byte, off int64) error {
	n, err := w.WriteAt(b, off)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// Commit compacts this layer into a new sealed file without changing the source.
// It preserves holes and explicit zeros, so the result can be stacked on lowers.
func (f *File) Commit(path string) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return os.ErrClosed
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	d, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, d.Close())
		if err != nil {
			os.Remove(path)
		}
	}()
	h := f.h
	h.flags = 7
	h.index = 0
	h.count = 0
	if err = writeAt(d, h.encode(), 0); err != nil {
		return err
	}
	pos := int64(headerSize)
	mappings := f.mappings.all()
	index := make([]mapping, 0, len(mappings))
	buf := make([]byte, 128*1024)
	for _, m := range mappings {
		out := m
		out.physical = uint64(pos / SectorSize)
		if !m.zero {
			src := int64(m.physical * SectorSize)
			left := int64(m.length * SectorSize)
			for left > 0 {
				n := min(left, int64(len(buf)))
				if _, err = f.data.ReadAt(buf[:n], src); err != nil {
					return err
				}
				if err = writeAt(d, buf[:n], pos); err != nil {
					return err
				}
				pos += n
				src += n
				left -= n
			}
		}
		index = append(index, out)
	}
	h.index = uint64(pos)
	for len(index)%256 != 0 {
		index = append(index, mapping{offset: invalidOffset})
	}
	h.count = uint64(len(index))
	for _, m := range index {
		if err = writeAt(d, m.encode(), pos); err != nil {
			return err
		}
		pos += 16
	}
	h.flags = 6
	if err = writeAt(d, h.encode(), pos); err != nil {
		return err
	}
	return d.Sync()
}
