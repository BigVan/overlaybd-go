package lsmt

import (
	"sort"

	"github.com/google/btree"
)

// mappingIndex keeps writable intervals in a B-tree so a random overwrite moves
// only entries in a few nodes, not the whole image's index. Sealed layers retain
// a compact sorted slice. Callers provide synchronization.
type mappingIndex struct {
	tree   *btree.BTreeG[mapping]
	sealed []mapping
}

func (idx *mappingIndex) preceding(pos uint64) (m mapping, ok bool) {
	if idx.tree != nil {
		idx.tree.DescendLessOrEqual(mapping{offset: pos}, func(item mapping) bool {
			m, ok = item, true
			return false
		})
	}
	return
}

// next returns the mapping covering pos, or the first one after the hole at pos.
func (idx *mappingIndex) next(pos uint64) (m mapping, ok bool) {
	if idx.tree == nil {
		i := sort.Search(len(idx.sealed), func(i int) bool { return idx.sealed[i].end() > pos })
		if i < len(idx.sealed) {
			return idx.sealed[i], true
		}
		return
	}
	if m, ok = idx.preceding(pos); ok && m.end() > pos {
		return m, true
	}
	ok = false
	idx.tree.AscendGreaterOrEqual(mapping{offset: pos}, func(item mapping) bool {
		m, ok = item, true
		return false
	})
	return
}

// visit yields intersecting mappings in logical order. Returning false stops
// iteration; the callback must not mutate this index.
func (idx *mappingIndex) visit(start, end uint64, fn func(mapping) bool) {
	if start >= end {
		return
	}
	if idx.tree == nil {
		i := sort.Search(len(idx.sealed), func(i int) bool { return idx.sealed[i].end() > start })
		for ; i < len(idx.sealed) && idx.sealed[i].offset < end; i++ {
			if !fn(idx.sealed[i]) {
				return
			}
		}
		return
	}
	if m, ok := idx.preceding(start); ok && m.end() > start {
		if !fn(m) || m.end() >= end {
			return
		}
		start = m.end()
	}
	idx.tree.AscendGreaterOrEqual(mapping{offset: start}, func(m mapping) bool {
		return m.offset < end && fn(m)
	})
}

func (idx *mappingIndex) insert(m mapping) {
	if idx.tree == nil {
		idx.tree = btree.NewG(32, func(a, b mapping) bool { return a.offset < b.offset })
	}
	if old, ok := idx.tree.Get(m); ok && old.length == m.length {
		idx.tree.ReplaceOrInsert(m)
		return
	}
	// Most 4 KiB overwrites intersect one extent. Collect before mutation:
	// modifying a B-tree while iterating would invalidate the traversal.
	var local [8]mapping
	overlaps := local[:0]
	idx.visit(m.offset, m.end(), func(old mapping) bool {
		overlaps = append(overlaps, old)
		return true
	})
	for _, old := range overlaps {
		idx.tree.Delete(old)
		if old.offset < m.offset {
			left := old
			left.length = m.offset - old.offset
			idx.tree.ReplaceOrInsert(left)
		}
		if old.end() > m.end() {
			right := old
			delta := m.end() - old.offset
			right.offset = m.end()
			right.length -= delta
			if !right.zero {
				right.physical += delta
			}
			idx.tree.ReplaceOrInsert(right)
		}
	}
	idx.tree.ReplaceOrInsert(m)
}

func (idx *mappingIndex) all() []mapping {
	if idx.tree == nil {
		return idx.sealed
	}
	out := make([]mapping, 0, idx.tree.Len())
	idx.tree.Ascend(func(m mapping) bool {
		out = append(out, m)
		return true
	})
	return out
}
