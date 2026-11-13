package lsmt

import (
	"errors"
	"os"
	"sync"
)

// Stack owns its layers, ordered bottom to top, and an optional writable upper.
type Stack struct {
	mu     sync.RWMutex
	layers []*File
	upper  *File
	size   int64
	closed bool
}

func NewStack(lowers []*File, upper *File) (*Stack, error) {
	s := &Stack{layers: append([]*File(nil), lowers...), upper: upper}
	for _, f := range lowers {
		if f == nil || !f.ReadOnly() {
			return nil, errors.New("lsmt: lower layers must be read-only")
		}
	}
	if upper != nil {
		if upper.ReadOnly() {
			return nil, errors.New("lsmt: upper layer must be writable")
		}
		s.layers = append(s.layers, upper)
	}
	if len(s.layers) == 0 {
		return nil, errors.New("lsmt: at least one layer is required")
	}
	s.size = s.layers[0].Size()
	for _, f := range s.layers {
		if f.Size() != s.size {
			return nil, errors.New("lsmt: all layers must have the same virtual size")
		}
	}
	return s, nil
}
func (s *Stack) Size() int64    { return s.size }
func (s *Stack) ReadOnly() bool { return s.upper == nil }
func (s *Stack) ReadAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	n, err := readBounds(p, off, s.size)
	if n == 0 {
		return n, err
	}
	clear(p[:n])
	for _, f := range s.layers {
		f.mu.RLock()
		var e error
		if f.closed {
			e = os.ErrClosed
		} else {
			e = f.overlay(p[:n], off)
		}
		f.mu.RUnlock()
		if e != nil {
			return 0, e
		}
	}
	return n, err
}
func (s *Stack) WriteAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	if s.upper == nil {
		return 0, errors.New("lsmt: read-only stack")
	}
	return s.upper.WriteAt(p, off)
}
func (s *Stack) Zero(off, length int64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return os.ErrClosed
	}
	if s.upper == nil {
		return errors.New("lsmt: read-only stack")
	}
	return s.upper.Zero(off, length)
}

// BindIOThread forwards the queue thread binding to the writable upper only.
// Read-only stacks and lower layers do not need a writable I/O thread binding.
func (s *Stack) BindIOThread() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	if s.upper == nil {
		return nil
	}
	return s.upper.BindIOThread()
}

func (s *Stack) Sync() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return os.ErrClosed
	}
	if s.upper == nil {
		return nil
	}
	return s.upper.Sync()
}
func (s *Stack) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	for _, f := range s.layers {
		err = errors.Join(err, f.Close())
	}
	return err
}
