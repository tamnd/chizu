package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Graph segments, doc 04 section 6: one partition of one graph generation,
// sources as a varint-delta CSR. Nodes are dense u40 ids assigned per
// generation. Blocks target 16 MiB raw, hold whole sources, and take zstd
// level 1 on top; deltas compress well regardless, the trade against
// WebGraph-class codecs is decode simplicity, and a PageRank iteration
// streams every block exactly once.

// MaxNodeID caps the dense id space: u40 covers 1T nodes.
const MaxNodeID = 1<<40 - 1

// graphBlockSize is one footer block entry: offset u64, storedlen u32,
// rawlen u32, first_src u40, crc u32.
const graphBlockSize = 25

func appendU40(dst []byte, v uint64) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32))
}

func u40(p []byte) uint64 {
	return uint64(p[0]) | uint64(p[1])<<8 | uint64(p[2])<<16 | uint64(p[3])<<24 | uint64(p[4])<<32
}

// GraphSegmentWriter accumulates sources and seals them into one segment.
// Sources must arrive strictly increasing; destinations must be sorted and
// strictly increasing per source. Both are format contracts, not something
// Seal repairs.
type GraphSegmentWriter struct {
	Generation  uint32
	Part        uint16
	Writer      uint64
	BlockTarget int

	srcs   []uint64
	dsts   [][]uint64
	nedges uint64
}

// Add appends one source and its sorted destinations; empty is legal (a
// page with no outlinks still owns its node id).
func (w *GraphSegmentWriter) Add(src uint64, dsts []uint64) error {
	if src > MaxNodeID {
		return fmt.Errorf("coldfmt: source %d exceeds u40", src)
	}
	if len(w.srcs) > 0 && src <= w.srcs[len(w.srcs)-1] {
		return errors.New("coldfmt: sources not strictly increasing")
	}
	for i, d := range dsts {
		if d > MaxNodeID {
			return fmt.Errorf("coldfmt: destination %d exceeds u40", d)
		}
		if i > 0 && d <= dsts[i-1] {
			return errors.New("coldfmt: destinations not strictly increasing")
		}
	}
	w.srcs = append(w.srcs, src)
	w.dsts = append(w.dsts, dsts)
	w.nedges += uint64(len(dsts))
	return nil
}

func (w *GraphSegmentWriter) NSrc() int { return len(w.srcs) }

func (w *GraphSegmentWriter) Seal() ([]byte, error) {
	if len(w.srcs) == 0 {
		return nil, errors.New("coldfmt: empty graph segment")
	}

	codec, err := newBlockCodecLevel(nil, zstd.SpeedFastest)
	if err != nil {
		return nil, err
	}
	defer codec.close()

	target := w.BlockTarget
	if target == 0 {
		target = BlockRawTarget
	}
	out := AppendHeader(make([]byte, 0, 1<<20), FormatGraphSeg, w.Writer)
	var index []blockEntry
	var buf []byte
	firstsrc := w.srcs[0]
	flush := func(next int) {
		offset := uint64(len(out))
		var storedlen uint32
		out, storedlen = codec.appendBlock(out, buf, CompZstd)
		index = append(index, blockEntry{
			offset:    offset,
			storedlen: storedlen,
			rawlen:    uint32(len(buf)),
			firstrow:  firstsrc,
			crc:       binary.LittleEndian.Uint32(out[len(out)-4:]),
		})
		buf = nil
		if next < len(w.srcs) {
			firstsrc = w.srcs[next]
		}
	}
	for i, src := range w.srcs {
		// The source delta chain restarts at every block boundary, so a
		// block decodes alone; the first source is stored absolute.
		if len(buf) == 0 {
			buf = AppendUvarint(buf, src)
		} else {
			buf = AppendUvarint(buf, src-w.srcs[i-1])
		}
		buf = AppendUvarint(buf, uint64(len(w.dsts[i])))
		prev := uint64(0)
		for j, d := range w.dsts[i] {
			if j == 0 {
				buf = AppendUvarint(buf, d)
			} else {
				buf = AppendUvarint(buf, d-prev)
			}
			prev = d
		}
		if len(buf) >= target && i+1 < len(w.srcs) {
			flush(i + 1)
		}
	}
	flush(len(w.srcs))

	footerOff := uint64(len(out))
	f := binary.LittleEndian.AppendUint32(nil, w.Generation)
	f = binary.LittleEndian.AppendUint16(f, w.Part)
	f = appendU40(f, w.srcs[0])
	f = appendU40(f, w.srcs[len(w.srcs)-1])
	f = binary.LittleEndian.AppendUint64(f, uint64(len(w.srcs)))
	f = binary.LittleEndian.AppendUint64(f, w.nedges)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(index)))
	for _, b := range index {
		f = binary.LittleEndian.AppendUint64(f, b.offset)
		f = binary.LittleEndian.AppendUint32(f, b.storedlen)
		f = binary.LittleEndian.AppendUint32(f, b.rawlen)
		f = appendU40(f, b.firstrow)
		f = binary.LittleEndian.AppendUint32(f, b.crc)
	}
	f = binary.LittleEndian.AppendUint32(f, CRC(f))
	out = append(out, f...)
	return AppendTail(out, footerOff, uint32(len(f))), nil
}

// GraphSegment is an opened graph segment.
type GraphSegment struct {
	Header     Header
	Generation uint32
	Part       uint16
	FirstSrc   uint64
	LastSrc    uint64
	NSrc       uint64
	NEdges     uint64

	blocks []blockEntry
	data   []byte
	codec  *blockCodec
}

func OpenGraphSegment(data []byte) (*GraphSegment, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatGraphSeg {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a graph segment", h.Format)
	}
	footerOff, footerLen, err := ParseTail(data)
	if err != nil {
		return nil, err
	}
	end := uint64(len(data) - TailSize)
	if footerOff > end || footerOff+uint64(footerLen) != end {
		return nil, errors.New("coldfmt: footer extent out of bounds")
	}
	f := data[footerOff:end]
	if len(f) < 36+graphBlockSize+4 {
		return nil, errors.New("coldfmt: footer too short")
	}
	if binary.LittleEndian.Uint32(f[len(f)-4:]) != CRC(f[:len(f)-4]) {
		return nil, errors.New("coldfmt: footer crc mismatch")
	}
	s := &GraphSegment{Header: h, data: data}
	s.Generation = binary.LittleEndian.Uint32(f)
	s.Part = binary.LittleEndian.Uint16(f[4:])
	s.FirstSrc = u40(f[6:])
	s.LastSrc = u40(f[11:])
	s.NSrc = binary.LittleEndian.Uint64(f[16:])
	s.NEdges = binary.LittleEndian.Uint64(f[24:])
	nblocks := binary.LittleEndian.Uint32(f[32:])
	if s.NSrc == 0 || s.FirstSrc > s.LastSrc {
		return nil, errors.New("coldfmt: source range out of order")
	}
	if s.NSrc > s.LastSrc-s.FirstSrc+1 {
		return nil, errors.New("coldfmt: source count exceeds source range")
	}
	if uint64(len(f)-36-4) != uint64(nblocks)*graphBlockSize || nblocks == 0 {
		return nil, errors.New("coldfmt: block index length mismatch")
	}
	pos := 36
	s.blocks = make([]blockEntry, nblocks)
	for i := range s.blocks {
		s.blocks[i] = blockEntry{
			offset:    binary.LittleEndian.Uint64(f[pos:]),
			storedlen: binary.LittleEndian.Uint32(f[pos+8:]),
			rawlen:    binary.LittleEndian.Uint32(f[pos+12:]),
			firstrow:  u40(f[pos+16:]),
			crc:       binary.LittleEndian.Uint32(f[pos+21:]),
		}
		pos += graphBlockSize
		if i == 0 && s.blocks[i].firstrow != s.FirstSrc {
			return nil, errors.New("coldfmt: first block does not start at first source")
		}
		if i > 0 && s.blocks[i].firstrow <= s.blocks[i-1].firstrow {
			return nil, errors.New("coldfmt: block first sources not increasing")
		}
		if s.blocks[i].firstrow > s.LastSrc {
			return nil, errors.New("coldfmt: block first source past last source")
		}
	}
	s.codec, err = newBlockCodec(nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *GraphSegment) Close() { s.codec.close() }

// Walk streams every source in id order with its destinations; the dsts
// slice is reused between calls. This is the PageRank access pattern: every
// block of every part exactly once.
func (s *GraphSegment) Walk(fn func(src uint64, dsts []uint64) error) error {
	var nsrc, nedges uint64
	var dsts []uint64
	prevSrc := uint64(0)
	for bi, b := range s.blocks {
		if b.offset > uint64(len(s.data)) ||
			uint64(b.storedlen) > uint64(len(s.data))-b.offset-blockHeaderSize-4 {
			return errors.New("coldfmt: block extent out of bounds")
		}
		raw, err := s.codec.decodeBlock(s.data[b.offset : b.offset+blockHeaderSize+uint64(b.storedlen)+4])
		if err != nil {
			return err
		}
		if uint32(len(raw)) != b.rawlen {
			return errors.New("coldfmt: block rawlen disagrees with index")
		}
		pos := 0
		blockStart := true
		for pos < len(raw) {
			v, n, err := Uvarint(raw[pos:])
			if err != nil {
				return err
			}
			pos += n
			var src uint64
			if blockStart {
				src = v
				if src != b.firstrow {
					return errors.New("coldfmt: block first source disagrees with index")
				}
				if bi > 0 && src <= prevSrc {
					return errors.New("coldfmt: sources not increasing across blocks")
				}
				blockStart = false
			} else {
				if v == 0 || v > MaxNodeID-prevSrc {
					return errors.New("coldfmt: source delta out of range")
				}
				src = prevSrc + v
			}
			if src > s.LastSrc {
				return errors.New("coldfmt: source past footer last source")
			}
			prevSrc = src
			deg, n, err := Uvarint(raw[pos:])
			if err != nil {
				return err
			}
			pos += n
			// Every destination costs at least one byte, so the remaining
			// bytes cap the degree before any allocation.
			if deg > uint64(len(raw)-pos) {
				return errors.New("coldfmt: degree exceeds block remainder")
			}
			dsts = dsts[:0]
			prevDst := uint64(0)
			for j := range deg {
				v, n, err := Uvarint(raw[pos:])
				if err != nil {
					return err
				}
				pos += n
				var d uint64
				if j == 0 {
					d = v
				} else {
					if v == 0 || v > MaxNodeID-prevDst {
						return errors.New("coldfmt: destination delta out of range")
					}
					d = prevDst + v
				}
				if d > MaxNodeID {
					return errors.New("coldfmt: destination exceeds u40")
				}
				prevDst = d
				dsts = append(dsts, d)
			}
			nsrc++
			nedges += deg
			if err := fn(src, dsts); err != nil {
				return err
			}
		}
	}
	if nsrc != s.NSrc || nedges != s.NEdges {
		return errors.New("coldfmt: walked counts disagree with footer")
	}
	return nil
}
