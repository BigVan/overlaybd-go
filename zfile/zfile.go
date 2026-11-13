// Package zfile implements the local, dictionary-free OverlayBD ZFile format.
// Format reference: containerd/overlaybd/src/overlaybd/zfile/{zfile.cpp,compressor.h}.
package zfile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

const (
	headerSize    = 512
	maxBlocks     = 8 * 1024 * 1024
	flagHeader    = 1
	flagData      = 2
	flagSealed    = 4
	flagOverwrite = 8
	flagDigest    = 16
)

var magic = []byte{0x5a, 0x46, 0x69, 0x6c, 0x65, 0, 1, 0, 0x74, 0x75, 0x6a, 0x69, 0x2e, 0x79, 0x79, 0x66, 0x40, 0x41, 0x6c, 0x69, 0x62, 0x61, 0x62, 0x61}
var le = binary.LittleEndian
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// OverlayBD uses the raw CRC32C accumulator (without initial/final complement).
func checksum(b []byte, seed uint32) uint32 { return ^crc32.Update(^seed, crcTable, b) }

// IsMagic reports whether b starts with the ZFile signature.
func IsMagic(b []byte) bool { return len(b) >= 8 && bytes.Equal(b[:8], magic[:8]) }

type metadata struct {
	flags, index, count, size uint64
	blockSize                 uint32
	algo, verify              byte
	indexCRC                  uint32
}

func parseHeader(b []byte, header bool) (metadata, error) {
	var h metadata
	if len(b) != headerSize || !bytes.Equal(b[:24], magic) || le.Uint32(b[24:]) != 96 {
		return h, errors.New("zfile: invalid signature or header size")
	}
	h.flags = le.Uint64(b[32:])
	if (h.flags&flagHeader != 0) != header || h.flags&flagData == 0 || h.flags & ^uint64(31) != 0 {
		return h, errors.New("zfile: unsupported header flags (compressed index is not supported)")
	}
	if h.flags&flagDigest != 0 {
		saved := le.Uint32(b[28:])
		copyBuf := append([]byte(nil), b...)
		clear(copyBuf[28:32])
		if checksum(copyBuf, 0) != saved {
			return h, errors.New("zfile: header checksum mismatch")
		}
	}
	h.index, h.count, h.size = le.Uint64(b[40:]), le.Uint64(b[48:]), le.Uint64(b[56:])
	h.indexCRC, h.blockSize = le.Uint32(b[64:]), le.Uint32(b[72:])
	h.algo, h.verify = b[76], b[88]
	if h.blockSize < 4096 || h.blockSize > 65536 || h.blockSize&(h.blockSize-1) != 0 {
		return h, errors.New("zfile: block size must be a power of two from 4096 to 65536")
	}
	if h.algo != 1 && h.algo != 2 {
		return h, fmt.Errorf("zfile: unsupported algorithm %d", h.algo)
	}
	if b[78] != 0 || le.Uint32(b[84:]) != 0 {
		return h, errors.New("zfile: dictionaries are not supported")
	}
	if h.verify > 1 {
		return h, errors.New("zfile: unsupported checksum mode")
	}
	return h, nil
}

func (h metadata) encode() []byte {
	b := make([]byte, headerSize)
	copy(b, magic)
	le.PutUint32(b[24:], 96)
	le.PutUint64(b[32:], h.flags)
	le.PutUint64(b[40:], h.index)
	le.PutUint64(b[48:], h.count)
	le.PutUint64(b[56:], h.size)
	le.PutUint32(b[64:], h.indexCRC)
	le.PutUint32(b[72:], h.blockSize)
	b[76], b[88] = h.algo, h.verify
	le.PutUint32(b[28:], checksum(b, 0))
	return b
}

// Reader decompresses only blocks touched by ReadAt. It is safe for concurrent use.
// NewReader does not take ownership of its source; Open takes ownership of the file.
type Reader struct {
	mu      sync.Mutex
	src     io.ReaderAt
	closer  io.Closer
	h       metadata
	offsets []int64
	decoder *zstd.Decoder
	closed  bool
}

func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReader(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	r.closer = f
	return r, nil
}

func NewReader(src io.ReaderAt, size int64) (*Reader, error) {
	if size < 2*headerSize {
		return nil, errors.New("zfile: truncated file")
	}
	b := make([]byte, headerSize)
	if _, err := src.ReadAt(b, 0); err != nil {
		return nil, err
	}
	header, err := parseHeader(b, true)
	if err != nil {
		return nil, err
	}
	if _, err = src.ReadAt(b, size-headerSize); err != nil {
		return nil, err
	}
	h, err := parseHeader(b, false)
	if err != nil {
		return nil, err
	}
	if h.flags&flagSealed == 0 || header.blockSize != h.blockSize || header.algo != h.algo || header.verify != h.verify {
		return nil, errors.New("zfile: incomplete or inconsistent file")
	}
	if header.flags&flagOverwrite != 0 && (header.index != h.index || header.count != h.count || header.size != h.size || header.indexCRC != h.indexCRC) {
		return nil, errors.New("zfile: overwritten header disagrees with trailer")
	}
	if h.size > math.MaxInt64 || h.count > maxBlocks || h.count != (h.size+uint64(h.blockSize)-1)/uint64(h.blockSize) ||
		h.index < headerSize || h.index > uint64(size-headerSize) || h.count*4 != uint64(size-headerSize)-h.index {
		return nil, errors.New("zfile: invalid index bounds or block count")
	}
	index := make([]byte, h.count*4)
	if len(index) > 0 {
		if _, err = src.ReadAt(index, int64(h.index)); err != nil {
			return nil, err
		}
	}
	if h.flags&flagDigest != 0 && checksum(index, 0) != h.indexCRC {
		return nil, errors.New("zfile: index checksum mismatch")
	}
	offsets := make([]int64, h.count+1)
	offsets[0] = headerSize
	for i := 0; i < int(h.count); i++ {
		n := le.Uint32(index[i*4:])
		if n <= uint32(h.verify)*4 || n > h.blockSize+1024 {
			return nil, errors.New("zfile: invalid compressed block length")
		}
		offsets[i+1] = offsets[i] + int64(n)
		if offsets[i+1] > int64(h.index) {
			return nil, errors.New("zfile: block overlaps index")
		}
	}
	if offsets[len(offsets)-1] != int64(h.index) {
		return nil, errors.New("zfile: invalid data length")
	}
	r := &Reader{src: src, h: h, offsets: offsets}
	if h.algo == 2 {
		r.decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecodeAllCapLimit(true))
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Reader) Size() int64 { return int64(r.h.size) }

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, errors.New("zfile: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.Size() {
		return 0, io.EOF
	}
	want := len(p)
	if int64(len(p)) > r.Size()-off {
		p = p[:r.Size()-off]
	}
	n := 0
	for n < len(p) {
		block := (off + int64(n)) / int64(r.h.blockSize)
		compressed := make([]byte, r.offsets[block+1]-r.offsets[block])
		if _, err := r.src.ReadAt(compressed, r.offsets[block]); err != nil {
			return n, err
		}
		if r.h.verify != 0 {
			payload := compressed[:len(compressed)-4]
			if checksum(payload, 100007) != le.Uint32(compressed[len(payload):]) {
				return n, fmt.Errorf("zfile: block %d checksum mismatch", block)
			}
			compressed = payload
		}
		expected := min(int64(r.h.blockSize), r.Size()-block*int64(r.h.blockSize))
		raw := make([]byte, expected)
		if r.h.algo == 1 {
			got, err := lz4.UncompressBlock(compressed, raw)
			if err != nil || got != len(raw) {
				return n, fmt.Errorf("zfile: invalid LZ4 block %d (decoded %d): %v", block, got, err)
			}
		} else {
			decoded, err := r.decoder.DecodeAll(compressed, raw[:0])
			if err != nil || len(decoded) != len(raw) {
				return n, fmt.Errorf("zfile: invalid ZSTD block %d: %v", block, err)
			}
			raw = decoded
		}
		start := (off + int64(n)) % int64(r.h.blockSize)
		n += copy(p[n:], raw[start:])
	}
	if n < want {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.decoder != nil {
		r.decoder.Close()
	}
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

type Options struct {
	// Algorithm is "lz4" (default) or "zstd"; BlockSize defaults to 4096.
	Algorithm string
	BlockSize int
}

// Compress writes a ZFile with header, block and index checksums. dst must be empty.
func Compress(dst io.Writer, src io.Reader, opt Options) error {
	if opt.BlockSize == 0 {
		opt.BlockSize = 4096
	}
	if opt.BlockSize < 4096 || opt.BlockSize > 65536 || opt.BlockSize&(opt.BlockSize-1) != 0 {
		return errors.New("zfile: invalid block size")
	}
	algo := byte(1)
	if opt.Algorithm == "zstd" {
		algo = 2
	} else if opt.Algorithm != "" && opt.Algorithm != "lz4" {
		return errors.New("zfile: algorithm must be lz4 or zstd")
	}
	h := metadata{flags: flagHeader | flagData | flagDigest, blockSize: uint32(opt.BlockSize), algo: algo, verify: 1}
	if err := writeFull(dst, h.encode()); err != nil {
		return err
	}
	var encoder *zstd.Encoder
	if algo == 2 {
		var err error
		encoder, err = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
		if err != nil {
			return err
		}
		defer encoder.Close()
	}
	buf := make([]byte, opt.BlockSize)
	index := make([]byte, 0)
	h.index = headerSize
	for {
		n, err := io.ReadFull(src, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		if n == 0 {
			break
		}
		if h.count >= maxBlocks {
			return errors.New("zfile: too many blocks")
		}
		var compressed []byte
		if algo == 1 {
			compressed = make([]byte, lz4.CompressBlockBound(n))
			written, e := lz4.CompressBlock(buf[:n], compressed, nil)
			if e != nil {
				return e
			}
			if written == 0 {
				compressed = literalBlock(buf[:n])
			} else {
				compressed = compressed[:written]
			}
		} else {
			compressed = encoder.EncodeAll(buf[:n], nil)
		}
		crc := checksum(compressed, 100007)
		compressed = le.AppendUint32(compressed, crc)
		if e := writeFull(dst, compressed); e != nil {
			return e
		}
		index = le.AppendUint32(index, uint32(len(compressed)))
		h.index += uint64(len(compressed))
		h.count++
		h.size += uint64(n)
		if err != nil {
			break
		}
	}
	if err := writeFull(dst, index); err != nil {
		return err
	}
	h.indexCRC = checksum(index, 0)
	h.flags = flagData | flagSealed | flagDigest
	return writeFull(dst, h.encode())
}

// LZ4's Go compressor may return zero for incompressible input. ZFile stores
// raw LZ4 blocks, so encode a literal-only sequence, never unframed source bytes.
func literalBlock(p []byte) []byte {
	n := len(p)
	b := []byte{byte(min(n, 15) << 4)}
	if n >= 15 {
		left := n - 15
		for left >= 255 {
			b = append(b, 255)
			left -= 255
		}
		b = append(b, byte(left))
	}
	return append(b, p...)
}

func writeFull(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
