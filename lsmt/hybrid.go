package lsmt

import (
	"errors"
	"sync"
)

// Hybrid payloads are mutable. Striped range locks let unrelated data I/O run
// concurrently while coordinating overlapping reads, writes and zero mappings.
// All stripes are acquired in increasing order, after File.mu and before logMu
// or indexMu. Hash collisions only reduce concurrency; they cannot lose updates.
type rangeLocks [4096]sync.RWMutex

type rangeGuard struct {
	locks      *rangeLocks
	a, b, c, d int
	write      bool
}

func (locks *rangeLocks) lock(off, length int64, write bool) rangeGuard {
	g := rangeGuard{locks: locks, write: write}
	if length == 0 {
		return g
	}
	first := off / 4096
	count := (off+length-1)/4096 - first + 1
	if count >= int64(len(locks)) {
		g.b = len(locks)
	} else {
		g.a = int(first % int64(len(locks)))
		g.b = g.a + int(count)
		if g.b > len(locks) {
			g.c, g.d = g.a, len(locks)
			g.a, g.b = 0, g.b-len(locks)
		}
	}
	for i := g.a; i < g.b; i++ {
		if write {
			locks[i].Lock()
		} else {
			locks[i].RLock()
		}
	}
	for i := g.c; i < g.d; i++ {
		if write {
			locks[i].Lock()
		} else {
			locks[i].RLock()
		}
	}
	return g
}

func (g rangeGuard) unlock() {
	for i := g.d - 1; i >= g.c; i-- {
		if g.write {
			g.locks[i].Unlock()
		} else {
			g.locks[i].RUnlock()
		}
	}
	for i := g.b - 1; i >= g.a; i-- {
		if g.write {
			g.locks[i].Unlock()
		} else {
			g.locks[i].RUnlock()
		}
	}
}

// The caller holds the lifecycle barrier and logical range write locks.
func (f *File) writeHybrid(p []byte, off int64) (int, error) {
	written := 0
	for len(p) > 0 {
		f.indexMu.RLock()
		m, ok := f.mappings.next(uint64(off / SectorSize))
		f.indexMu.RUnlock()
		end := off + int64(min(len(p), maxLength*SectorSize))
		if !ok || int64(m.offset*SectorSize) > off {
			if ok {
				end = min(end, int64(m.offset*SectorSize))
			}
			n, err := f.appendData(p[:end-off], off)
			if err != nil {
				return written + n, err
			}
		} else {
			end = min(end, int64(m.end()*SectorSize))
			physical := int64(m.physical*SectorSize) + off - int64(m.offset*SectorSize)
			// Like upstream's direct hybrid path, sub-block mappings move to a
			// fresh extent rather than sharing a physical read-modify-write block.
			if m.zero || (f.direct && (physical%4096 != 0 || (end-off)%4096 != 0)) {
				n, err := f.appendData(p[:end-off], off)
				if err != nil {
					return written + n, err
				}
			} else {
				if err := writeAt(f.dataFile, p[:end-off], physical); err != nil {
					f.logMu.Lock()
					f.writeErr = errors.Join(f.writeErr, err)
					f.logMu.Unlock()
					return written, err
				}
			}
		}
		n := int(end - off)
		written += n
		p, off = p[n:], end
	}
	return written, nil
}
