package coldfmt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Raw HTML segments, doc 04 section 4: the page segment's block-and-footer
// shape with a single column of decoded HTML, plus a (urlfp, off, len) doc
// index in the footer. A separate family so the retention knob is a
// manifest decision that never rewrites page segments. Page rows point in
// here via RawRef{Seq, Off, Len}; off and len address the decoded stream.

// rawDocIndexSize is one doc index entry: urlfp 16, off u32, len u32.
const rawDocIndexSize = 24

// RawDoc is one entry of the footer's doc index.
type RawDoc struct {
	URLFP [16]byte
	Off   uint32
	Len   uint32
}

// RawSegmentWriter accumulates decoded HTML docs and seals them into one
// segment. Blocks break only between docs, so every doc lives inside a
// single block and one ranged read serves one RawRef.
type RawSegmentWriter struct {
	Partition   uint16
	Epoch       uint32
	Seq         uint64
	Writer      uint64
	BlockTarget int
	docs        []RawDoc
	html        [][]byte
	total       uint64
}

func (w *RawSegmentWriter) Add(fp [16]byte, html []byte) {
	w.docs = append(w.docs, RawDoc{URLFP: fp, Off: uint32(w.total), Len: uint32(len(html))})
	w.html = append(w.html, html)
	w.total += uint64(len(html))
}

func (w *RawSegmentWriter) NDocs() int { return len(w.docs) }

func (w *RawSegmentWriter) Seal() ([]byte, error) {
	if len(w.docs) == 0 {
		return nil, errors.New("coldfmt: empty raw segment")
	}
	if len(w.docs) > maxSegmentRows {
		return nil, fmt.Errorf("coldfmt: %d docs exceeds segment cap", len(w.docs))
	}
	// Off/Len are u32 because RawRef carries them as u32; a segment whose
	// decoded stream outgrows that must have been split long before here.
	if w.total > math.MaxUint32 {
		return nil, fmt.Errorf("coldfmt: raw stream %d bytes exceeds u32 addressing", w.total)
	}

	// Plain zstd, no per-segment dictionary: the zstd-dict lab's server3
	// verdict retired training at 16 MiB blocks (0.09% gain on real CC
	// text) and raw HTML blocks carry even more in-window redundancy.
	codec, err := newBlockCodec(nil)
	if err != nil {
		return nil, err
	}
	defer codec.close()

	target := w.BlockTarget
	if target == 0 {
		target = BlockRawTarget
	}
	out := AppendHeader(make([]byte, 0, int(w.total)/2+1<<16), FormatRawHTML, w.Writer)
	var index []blockEntry
	var buf []byte
	firstdoc := 0
	flush := func(end int) {
		offset := uint64(len(out))
		var storedlen uint32
		out, storedlen = codec.appendBlock(out, buf, CompZstd)
		index = append(index, blockEntry{
			offset:    offset,
			storedlen: storedlen,
			rawlen:    uint32(len(buf)),
			firstrow:  uint64(firstdoc),
			crc:       binary.LittleEndian.Uint32(out[len(out)-4:]),
		})
		buf, firstdoc = nil, end
	}
	for i, h := range w.html {
		buf = append(buf, h...)
		if len(buf) >= target && i+1 < len(w.html) {
			flush(i + 1)
		}
	}
	flush(len(w.html))

	// Dictionary slot kept at zero length, same as page segments.
	dictOff := uint64(len(out))

	footerOff := uint64(len(out))
	footer := w.encodeFooter(index, dictOff, 0)
	out = append(out, footer...)
	return AppendTail(out, footerOff, uint32(len(footer))), nil
}

func (w *RawSegmentWriter) encodeFooter(index []blockEntry, dictOff uint64, dictLen uint32) []byte {
	f := binary.LittleEndian.AppendUint16(nil, w.Partition)
	f = binary.LittleEndian.AppendUint32(f, w.Epoch)
	f = binary.LittleEndian.AppendUint64(f, w.Seq)
	f = binary.LittleEndian.AppendUint64(f, uint64(len(w.docs)))
	f = binary.LittleEndian.AppendUint32(f, uint32(len(index)))
	for _, b := range index {
		f = binary.LittleEndian.AppendUint64(f, b.offset)
		f = binary.LittleEndian.AppendUint32(f, b.storedlen)
		f = binary.LittleEndian.AppendUint32(f, b.rawlen)
		f = binary.LittleEndian.AppendUint64(f, b.firstrow)
		f = binary.LittleEndian.AppendUint32(f, b.crc)
	}
	f = binary.LittleEndian.AppendUint64(f, dictOff)
	f = binary.LittleEndian.AppendUint32(f, dictLen)
	for _, d := range w.docs {
		f = append(f, d.URLFP[:]...)
		f = binary.LittleEndian.AppendUint32(f, d.Off)
		f = binary.LittleEndian.AppendUint32(f, d.Len)
	}
	return binary.LittleEndian.AppendUint32(f, CRC(f))
}

// RawSegment is an opened raw HTML segment.
type RawSegment struct {
	Header    Header
	Partition uint16
	Epoch     uint32
	Seq       uint64

	docs   []RawDoc
	blocks []blockEntry
	data   []byte
	codec  *blockCodec
}

func OpenRawSegment(data []byte) (*RawSegment, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatRawHTML {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a raw segment", h.Format)
	}
	footerOff, footerLen, err := ParseTail(data)
	if err != nil {
		return nil, err
	}
	end := uint64(len(data) - TailSize)
	if footerOff > end || footerOff+uint64(footerLen) != end {
		return nil, errors.New("coldfmt: footer extent out of bounds")
	}
	s := &RawSegment{Header: h, data: data}
	dictOff, dictLen, err := s.parseFooter(data[footerOff:end])
	if err != nil {
		return nil, err
	}
	if dictOff > uint64(len(data)) || uint64(dictLen) > uint64(len(data))-dictOff {
		return nil, errors.New("coldfmt: dictionary extent out of bounds")
	}
	s.codec, err = newBlockCodec(data[dictOff : dictOff+uint64(dictLen)])
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *RawSegment) parseFooter(f []byte) (dictOff uint64, dictLen uint32, err error) {
	if len(f) < 26+12+4 {
		return 0, 0, errors.New("coldfmt: footer too short")
	}
	if binary.LittleEndian.Uint32(f[len(f)-4:]) != CRC(f[:len(f)-4]) {
		return 0, 0, errors.New("coldfmt: footer crc mismatch")
	}
	s.Partition = binary.LittleEndian.Uint16(f)
	s.Epoch = binary.LittleEndian.Uint32(f[2:])
	s.Seq = binary.LittleEndian.Uint64(f[6:])
	ndocs := binary.LittleEndian.Uint64(f[14:])
	nblocks := binary.LittleEndian.Uint32(f[22:])
	if ndocs == 0 || ndocs > maxSegmentRows {
		return 0, 0, fmt.Errorf("coldfmt: doc count %d out of range", ndocs)
	}
	pos := 26
	if nblocks == 0 || uint64(nblocks) > uint64(len(f)-pos)/28 {
		return 0, 0, errors.New("coldfmt: block count out of range")
	}
	s.blocks = make([]blockEntry, nblocks)
	for i := range s.blocks {
		s.blocks[i] = blockEntry{
			offset:    binary.LittleEndian.Uint64(f[pos:]),
			storedlen: binary.LittleEndian.Uint32(f[pos+8:]),
			rawlen:    binary.LittleEndian.Uint32(f[pos+12:]),
			firstrow:  binary.LittleEndian.Uint64(f[pos+16:]),
			crc:       binary.LittleEndian.Uint32(f[pos+24:]),
		}
		pos += 28
		if i == 0 && s.blocks[i].firstrow != 0 {
			return 0, 0, errors.New("coldfmt: first block does not start at doc 0")
		}
		if i > 0 && s.blocks[i].firstrow <= s.blocks[i-1].firstrow {
			return 0, 0, errors.New("coldfmt: block first docs not increasing")
		}
		if s.blocks[i].firstrow >= ndocs {
			return 0, 0, errors.New("coldfmt: block first doc past doc count")
		}
	}
	if len(f)-pos < 12+4 {
		return 0, 0, errors.New("coldfmt: footer truncated after block index")
	}
	dictOff = binary.LittleEndian.Uint64(f[pos:])
	dictLen = binary.LittleEndian.Uint32(f[pos+8:])
	pos += 12
	if uint64(len(f)-pos-4) != ndocs*rawDocIndexSize {
		return 0, 0, errors.New("coldfmt: doc index length mismatch")
	}
	s.docs = make([]RawDoc, ndocs)
	var stream uint64
	for i := range s.docs {
		copy(s.docs[i].URLFP[:], f[pos:])
		s.docs[i].Off = binary.LittleEndian.Uint32(f[pos+16:])
		s.docs[i].Len = binary.LittleEndian.Uint32(f[pos+20:])
		pos += rawDocIndexSize
		if uint64(s.docs[i].Off) != stream {
			return 0, 0, errors.New("coldfmt: doc index offsets not contiguous")
		}
		stream += uint64(s.docs[i].Len)
	}
	var blockSum uint64
	for _, b := range s.blocks {
		blockSum += uint64(b.rawlen)
	}
	if blockSum != stream {
		return 0, 0, errors.New("coldfmt: block raw sizes disagree with doc index")
	}
	return dictOff, dictLen, nil
}

// Docs exposes the footer's doc index in stream order.
func (s *RawSegment) Docs() []RawDoc { return s.docs }

func (s *RawSegment) Close() { s.codec.close() }

// HTML returns the decoded HTML for one doc index entry by decoding the
// single block that holds it; docs never span blocks by construction.
func (s *RawSegment) HTML(d RawDoc) ([]byte, error) {
	// Blocks partition the decoded stream in doc order, so the block
	// holding d starts at the stream offset of its first doc.
	bi := 0
	for i := 1; i < len(s.blocks); i++ {
		if s.docs[s.blocks[i].firstrow].Off <= d.Off {
			bi = i
		}
	}
	b := s.blocks[bi]
	if b.offset > uint64(len(s.data)) ||
		uint64(b.storedlen) > uint64(len(s.data))-b.offset-blockHeaderSize-4 {
		return nil, errors.New("coldfmt: block extent out of bounds")
	}
	raw, err := s.codec.decodeBlock(s.data[b.offset : b.offset+blockHeaderSize+uint64(b.storedlen)+4])
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) != b.rawlen {
		return nil, errors.New("coldfmt: block rawlen disagrees with index")
	}
	start := uint64(d.Off) - uint64(s.docs[b.firstrow].Off)
	if start+uint64(d.Len) > uint64(len(raw)) {
		return nil, errors.New("coldfmt: doc extent outside its block")
	}
	return raw[start : start+uint64(d.Len)], nil
}

// Get is the point lookup: linear over the doc index, which is fine at
// segment scale (~16-20k docs) for the admin paths that use it.
func (s *RawSegment) Get(fp [16]byte) ([]byte, bool, error) {
	for _, d := range s.docs {
		if bytes.Equal(d.URLFP[:], fp[:]) {
			h, err := s.HTML(d)
			return h, err == nil, err
		}
	}
	return nil, false, nil
}
