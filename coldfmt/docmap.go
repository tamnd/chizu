package coldfmt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Docmap segments, doc 04 section 7: the bridge between crawl identity and
// index identity, written per shard per build. Fixed 66-byte rows sorted by
// urlfp so every consumer (delta builds, tombstone derivation, alias
// resolution, admin point lookups) is a merge join or a binary search.

// DocmapRowSize is the frozen row width: urlfp 16, docid u64, quality u8,
// flags u8, sha256 32, fetch_ms u64.
const DocmapRowSize = 66

// docmapBlockSize is one footer block entry: offset u64, storedlen u32,
// rawlen u32, firstrow u64, firstfp 16, crc u32. firstfp is what lets a
// point lookup pick its block without decoding any other.
const docmapBlockSize = 44

// DocmapRow is one row.
type DocmapRow struct {
	URLFP   [16]byte
	DocID   uint64
	Quality uint8
	Flags   uint8
	SHA256  [32]byte
	FetchMS uint64
}

// DocmapWriter accumulates rows and seals them into one segment. Rows must
// arrive strictly increasing by urlfp; that is the format's sort contract,
// not something Seal repairs.
type DocmapWriter struct {
	Shard       uint16
	Epoch       uint32
	Seq         uint64
	Writer      uint64
	BlockTarget int
	rows        []DocmapRow
}

func (w *DocmapWriter) Add(r DocmapRow) { w.rows = append(w.rows, r) }

func (w *DocmapWriter) NRows() int { return len(w.rows) }

type docmapBlock struct {
	entry   blockEntry
	firstfp [16]byte
}

func (w *DocmapWriter) Seal() ([]byte, error) {
	if len(w.rows) == 0 {
		return nil, errors.New("coldfmt: empty docmap segment")
	}
	if len(w.rows) > maxSegmentRows {
		return nil, fmt.Errorf("coldfmt: %d rows exceeds segment cap", len(w.rows))
	}
	for i := 1; i < len(w.rows); i++ {
		if bytes.Compare(w.rows[i-1].URLFP[:], w.rows[i].URLFP[:]) >= 0 {
			return nil, errors.New("coldfmt: docmap rows not strictly increasing by urlfp")
		}
	}

	codec, err := newBlockCodec(nil)
	if err != nil {
		return nil, err
	}
	defer codec.close()

	target := w.BlockTarget
	if target == 0 {
		target = BlockRawTarget
	}
	perBlock := max(target/DocmapRowSize, 1)

	out := AppendHeader(make([]byte, 0, len(w.rows)*DocmapRowSize/2+1<<12), FormatDocmap, w.Writer)
	var blocks []docmapBlock
	for first := 0; first < len(w.rows); first += perBlock {
		n := min(perBlock, len(w.rows)-first)
		buf := make([]byte, 0, n*DocmapRowSize)
		for i := first; i < first+n; i++ {
			buf = appendDocmapRow(buf, &w.rows[i])
		}
		offset := uint64(len(out))
		var storedlen uint32
		out, storedlen = codec.appendBlock(out, buf, CompZstd)
		blocks = append(blocks, docmapBlock{
			entry: blockEntry{
				offset:    offset,
				storedlen: storedlen,
				rawlen:    uint32(len(buf)),
				firstrow:  uint64(first),
				crc:       binary.LittleEndian.Uint32(out[len(out)-4:]),
			},
			firstfp: w.rows[first].URLFP,
		})
	}

	footerOff := uint64(len(out))
	f := binary.LittleEndian.AppendUint16(nil, w.Shard)
	f = binary.LittleEndian.AppendUint32(f, w.Epoch)
	f = binary.LittleEndian.AppendUint64(f, w.Seq)
	f = binary.LittleEndian.AppendUint64(f, uint64(len(w.rows)))
	f = binary.LittleEndian.AppendUint32(f, uint32(len(blocks)))
	for _, b := range blocks {
		f = binary.LittleEndian.AppendUint64(f, b.entry.offset)
		f = binary.LittleEndian.AppendUint32(f, b.entry.storedlen)
		f = binary.LittleEndian.AppendUint32(f, b.entry.rawlen)
		f = binary.LittleEndian.AppendUint64(f, b.entry.firstrow)
		f = append(f, b.firstfp[:]...)
		f = binary.LittleEndian.AppendUint32(f, b.entry.crc)
	}
	f = binary.LittleEndian.AppendUint32(f, CRC(f))
	out = append(out, f...)
	return AppendTail(out, footerOff, uint32(len(f))), nil
}

func appendDocmapRow(dst []byte, r *DocmapRow) []byte {
	dst = append(dst, r.URLFP[:]...)
	dst = binary.LittleEndian.AppendUint64(dst, r.DocID)
	dst = append(dst, r.Quality, r.Flags)
	dst = append(dst, r.SHA256[:]...)
	return binary.LittleEndian.AppendUint64(dst, r.FetchMS)
}

func decodeDocmapRow(p []byte, r *DocmapRow) {
	copy(r.URLFP[:], p)
	r.DocID = binary.LittleEndian.Uint64(p[16:])
	r.Quality = p[24]
	r.Flags = p[25]
	copy(r.SHA256[:], p[26:])
	r.FetchMS = binary.LittleEndian.Uint64(p[58:])
}

// DocmapSegment is an opened docmap.
type DocmapSegment struct {
	Header Header
	Shard  uint16
	Epoch  uint32
	Seq    uint64
	NRows  uint64

	blocks []docmapBlock
	data   []byte
	codec  *blockCodec
}

func OpenDocmapSegment(data []byte) (*DocmapSegment, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatDocmap {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a docmap segment", h.Format)
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
	if len(f) < 26+docmapBlockSize+4 {
		return nil, errors.New("coldfmt: footer too short")
	}
	if binary.LittleEndian.Uint32(f[len(f)-4:]) != CRC(f[:len(f)-4]) {
		return nil, errors.New("coldfmt: footer crc mismatch")
	}
	s := &DocmapSegment{Header: h, data: data}
	s.Shard = binary.LittleEndian.Uint16(f)
	s.Epoch = binary.LittleEndian.Uint32(f[2:])
	s.Seq = binary.LittleEndian.Uint64(f[6:])
	s.NRows = binary.LittleEndian.Uint64(f[14:])
	nblocks := binary.LittleEndian.Uint32(f[22:])
	if s.NRows == 0 || s.NRows > maxSegmentRows {
		return nil, fmt.Errorf("coldfmt: row count %d out of range", s.NRows)
	}
	if uint64(len(f)-26-4) != uint64(nblocks)*docmapBlockSize || nblocks == 0 {
		return nil, errors.New("coldfmt: block index length mismatch")
	}
	pos := 26
	s.blocks = make([]docmapBlock, nblocks)
	for i := range s.blocks {
		s.blocks[i] = docmapBlock{entry: blockEntry{
			offset:    binary.LittleEndian.Uint64(f[pos:]),
			storedlen: binary.LittleEndian.Uint32(f[pos+8:]),
			rawlen:    binary.LittleEndian.Uint32(f[pos+12:]),
			firstrow:  binary.LittleEndian.Uint64(f[pos+16:]),
			crc:       binary.LittleEndian.Uint32(f[pos+40:]),
		}}
		copy(s.blocks[i].firstfp[:], f[pos+24:])
		pos += docmapBlockSize
		e := s.blocks[i].entry
		if i == 0 && e.firstrow != 0 {
			return nil, errors.New("coldfmt: first block does not start at row 0")
		}
		if i > 0 && e.firstrow <= s.blocks[i-1].entry.firstrow {
			return nil, errors.New("coldfmt: block first rows not increasing")
		}
		if i > 0 && bytes.Compare(s.blocks[i-1].firstfp[:], s.blocks[i].firstfp[:]) >= 0 {
			return nil, errors.New("coldfmt: block first fingerprints not increasing")
		}
		if e.firstrow >= s.NRows {
			return nil, errors.New("coldfmt: block first row past row count")
		}
	}
	var err2 error
	s.codec, err2 = newBlockCodec(nil)
	if err2 != nil {
		return nil, err2
	}
	return s, nil
}

func (s *DocmapSegment) Close() { s.codec.close() }

func (s *DocmapSegment) block(i int) ([]byte, error) {
	b := s.blocks[i].entry
	if b.offset > uint64(len(s.data)) ||
		uint64(b.storedlen) > uint64(len(s.data))-b.offset-blockHeaderSize-4 {
		return nil, errors.New("coldfmt: block extent out of bounds")
	}
	raw, err := s.codec.decodeBlock(s.data[b.offset : b.offset+blockHeaderSize+uint64(b.storedlen)+4])
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) != b.rawlen || len(raw)%DocmapRowSize != 0 {
		return nil, errors.New("coldfmt: block size disagrees with row width")
	}
	next := s.NRows
	if i+1 < len(s.blocks) {
		next = s.blocks[i+1].entry.firstrow
	}
	if uint64(len(raw)/DocmapRowSize) != next-b.firstrow {
		return nil, errors.New("coldfmt: block row count disagrees with index")
	}
	return raw, nil
}

// Rows decodes the whole segment; bulk consumers stream it as a merge join.
func (s *DocmapSegment) Rows() ([]DocmapRow, error) {
	rows := make([]DocmapRow, 0, s.NRows)
	var prev []byte
	for i := range s.blocks {
		raw, err := s.block(i)
		if err != nil {
			return nil, err
		}
		for p := 0; p < len(raw); p += DocmapRowSize {
			var r DocmapRow
			decodeDocmapRow(raw[p:], &r)
			if prev != nil && bytes.Compare(prev, r.URLFP[:]) >= 0 {
				return nil, errors.New("coldfmt: rows not strictly increasing by urlfp")
			}
			rows = append(rows, r)
			prev = rows[len(rows)-1].URLFP[:]
		}
	}
	if uint64(len(rows)) != s.NRows {
		return nil, errors.New("coldfmt: row count mismatch")
	}
	return rows, nil
}

// Find is the point lookup: pick the block by first fingerprint, decode
// just that block, binary search inside it.
func (s *DocmapSegment) Find(fp [16]byte) (DocmapRow, bool, error) {
	bi := sort.Search(len(s.blocks), func(i int) bool {
		return bytes.Compare(s.blocks[i].firstfp[:], fp[:]) > 0
	}) - 1
	if bi < 0 {
		return DocmapRow{}, false, nil
	}
	raw, err := s.block(bi)
	if err != nil {
		return DocmapRow{}, false, err
	}
	n := len(raw) / DocmapRowSize
	ri := sort.Search(n, func(i int) bool {
		return bytes.Compare(raw[i*DocmapRowSize:i*DocmapRowSize+16], fp[:]) >= 0
	})
	if ri == n || !bytes.Equal(raw[ri*DocmapRowSize:ri*DocmapRowSize+16], fp[:]) {
		return DocmapRow{}, false, nil
	}
	var r DocmapRow
	decodeDocmapRow(raw[ri*DocmapRowSize:], &r)
	return r, true, nil
}
