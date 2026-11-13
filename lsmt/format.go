// Package lsmt implements OverlayBD's append-only local LSMT layers.
// Format reference: containerd/overlaybd/src/overlaybd/lsmt/{file.cpp,index.h}.
package lsmt

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	SectorSize    = 512
	headerSize    = 4096
	maxLength     = (1 << 14) - 1
	invalidOffset = (1 << 50) - 1
	maxRecords    = 8 * 1024 * 1024
	flagHybrid    = uint32(1 << 5)
)

var le = binary.LittleEndian
var magic = []byte{0x4c, 0x53, 0x4d, 0x54, 0, 1, 2, 0, 0x65, 0x7e, 0x63, 0xd2, 0x94, 0x44, 0x08, 0x4c, 0xa2, 0xd2, 0xc8, 0xec, 0x4f, 0xcf, 0xae, 0x8a}

type header struct {
	flags              uint32
	index, count, size uint64
	uuid, parent       string
}

func newHeader(size int64) (header, error) {
	u := make([]byte, 16)
	if _, err := rand.Read(u); err != nil {
		return header{}, err
	}
	u[6] = u[6]&15 | 64
	u[8] = u[8]&63 | 128
	return header{size: uint64(size), uuid: fmt.Sprintf("%X-%X-%X-%X-%X", u[:4], u[4:6], u[6:8], u[8:10], u[10:]), parent: "00000000-0000-0000-0000-000000000000"}, nil
}

func (h header) encode() []byte {
	b := make([]byte, headerSize)
	copy(b, magic)
	le.PutUint32(b[24:], 390)
	le.PutUint32(b[28:], h.flags)
	le.PutUint64(b[32:], h.index)
	le.PutUint64(b[40:], h.count)
	le.PutUint64(b[48:], h.size)
	copy(b[56:93], h.uuid)
	copy(b[93:130], h.parent)
	b[132], b[133] = 1, 1
	return b
}

func readHeader(r io.ReaderAt, off int64) (header, error) {
	var h header
	b := make([]byte, headerSize)
	if _, err := r.ReadAt(b, off); err != nil {
		return h, err
	}
	if !bytes.Equal(b[:24], magic) || le.Uint32(b[24:]) != 390 || b[132] != 1 || b[133] > 1 {
		return h, errors.New("lsmt: unsupported header signature/version")
	}
	h.flags, h.index, h.count, h.size = le.Uint32(b[28:]), le.Uint64(b[32:]), le.Uint64(b[40:]), le.Uint64(b[48:])
	h.uuid, h.parent = string(bytes.TrimRight(b[56:93], "\x00")), string(bytes.TrimRight(b[93:130], "\x00"))
	if h.flags & ^(uint32(7)|flagHybrid) != 0 {
		return h, errors.New("lsmt: only append and hybrid layers are supported (no sparse/warp)")
	}
	if h.size == 0 || h.size > math.MaxInt64 || h.size%SectorSize != 0 || h.size/SectorSize >= invalidOffset {
		return h, errors.New("lsmt: invalid virtual size")
	}
	return h, nil
}

// All offsets and lengths in a mapping are measured in 512-byte sectors.
type mapping struct {
	offset, length, physical uint64
	zero                     bool
}

func (m mapping) end() uint64 { return m.offset + m.length }
func (m mapping) encode() []byte {
	b := make([]byte, 16)
	le.PutUint64(b, m.offset|m.length<<50)
	p := m.physical
	if m.zero {
		p |= 1 << 55
	}
	le.PutUint64(b[8:], p)
	return b
}
func decodeMapping(b []byte) mapping {
	a, p := le.Uint64(b), le.Uint64(b[8:])
	return mapping{offset: a & invalidOffset, length: a >> 50, physical: p & ((1 << 55) - 1), zero: p&(1<<55) != 0}
}

func loadIndex(r io.ReaderAt, offset, count uint64, size int64, dataEnd int64, sealed bool) (mappingIndex, error) {
	if count > maxRecords {
		return mappingIndex{}, errors.New("lsmt: index exceeds memory limit")
	}
	var index mappingIndex
	if sealed {
		index.sealed = make([]mapping, 0, count)
	}
	buf := make([]byte, 16*4096)
	for i := uint64(0); i < count; {
		n := min(count-i, uint64(len(buf)/16))
		if _, err := r.ReadAt(buf[:n*16], int64(offset+i*16)); err != nil {
			return mappingIndex{}, err
		}
		for j := uint64(0); j < n; j++ {
			m := decodeMapping(buf[j*16:])
			if m.offset == invalidOffset {
				continue
			}
			// Warp layers reuse this header but may point into an external file
			// via the tag byte. Never silently interpret those references locally.
			// This minimal implementation accepts untagged local mappings only.
			if buf[j*16+15] != 0 {
				return mappingIndex{}, errors.New("lsmt: tagged mappings are unsupported (warp or pre-tagged layer)")
			}
			if m.length == 0 || m.end() > uint64(size/SectorSize) || m.physical < 8 || m.physical > uint64(dataEnd/SectorSize) || (!m.zero && m.length > uint64(dataEnd/SectorSize)-m.physical) {
				return mappingIndex{}, fmt.Errorf("lsmt: invalid mapping at record %d", i+j)
			}
			if sealed {
				if len(index.sealed) > 0 && index.sealed[len(index.sealed)-1].end() > m.offset {
					return mappingIndex{}, errors.New("lsmt: sealed index is not ordered")
				}
				index.sealed = append(index.sealed, m)
			} else {
				index.insert(m)
			}
		}
		i += n
	}
	return index, nil
}
