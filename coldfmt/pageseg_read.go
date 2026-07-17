package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PageSegment is an opened segment. The open sequence is doc 04 section 2:
// tail, footer, then data by the footer's block index; no scan.
type PageSegment struct {
	Header       Header
	Partition    uint16
	Epoch        uint32
	Seq          uint64
	NRows        uint64
	FirstFetchMS uint64
	LastFetchMS  uint64

	data  []byte
	index [pageNCols][]blockEntry
	bloom []byte
	codec *blockCodec
}

// OpenPageSegment parses and verifies a whole segment held in memory.
// Readers that ranged-GET will open from the same footer; this entry point
// is the builder's and the test corpus's.
func OpenPageSegment(data []byte) (*PageSegment, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatPageSeg {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a page segment", h.Format)
	}
	footerOff, footerLen, err := ParseTail(data)
	if err != nil {
		return nil, err
	}
	end := uint64(len(data) - TailSize)
	if footerOff > end || uint64(footerLen) > end-footerOff || footerOff+uint64(footerLen) != end {
		return nil, errors.New("coldfmt: footer extent out of bounds")
	}
	s := &PageSegment{Header: h, data: data}
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

func (s *PageSegment) parseFooter(f []byte) (dictOff uint64, dictLen uint32, err error) {
	if len(f) < 42+12+4+4 {
		return 0, 0, errors.New("coldfmt: footer too short")
	}
	if binary.LittleEndian.Uint32(f[len(f)-4:]) != CRC(f[:len(f)-4]) {
		return 0, 0, errors.New("coldfmt: footer crc mismatch")
	}
	s.Partition = binary.LittleEndian.Uint16(f)
	s.Epoch = binary.LittleEndian.Uint32(f[2:])
	s.Seq = binary.LittleEndian.Uint64(f[6:])
	s.NRows = binary.LittleEndian.Uint64(f[14:])
	s.FirstFetchMS = binary.LittleEndian.Uint64(f[22:])
	s.LastFetchMS = binary.LittleEndian.Uint64(f[30:])
	if s.NRows == 0 || s.NRows > maxSegmentRows {
		return 0, 0, fmt.Errorf("coldfmt: row count %d out of range", s.NRows)
	}
	if ncols := binary.LittleEndian.Uint32(f[38:]); ncols != pageNCols {
		return 0, 0, fmt.Errorf("coldfmt: %d columns, want %d", ncols, pageNCols)
	}
	pos := 42
	for col := range pageNCols {
		if len(f)-pos < 6 {
			return 0, 0, errors.New("coldfmt: colindex truncated")
		}
		id := binary.LittleEndian.Uint16(f[pos:])
		nblocks := binary.LittleEndian.Uint32(f[pos+2:])
		pos += 6
		if int(id) != col {
			return 0, 0, fmt.Errorf("coldfmt: colindex id %d at position %d", id, col)
		}
		if nblocks == 0 || uint64(nblocks) > uint64(len(f)-pos)/28 {
			return 0, 0, errors.New("coldfmt: colindex block count out of range")
		}
		blocks := make([]blockEntry, nblocks)
		for i := range blocks {
			blocks[i] = blockEntry{
				offset:    binary.LittleEndian.Uint64(f[pos:]),
				storedlen: binary.LittleEndian.Uint32(f[pos+8:]),
				rawlen:    binary.LittleEndian.Uint32(f[pos+12:]),
				firstrow:  binary.LittleEndian.Uint64(f[pos+16:]),
				crc:       binary.LittleEndian.Uint32(f[pos+24:]),
			}
			pos += 28
			if i == 0 && blocks[i].firstrow != 0 {
				return 0, 0, errors.New("coldfmt: first block does not start at row 0")
			}
			if i > 0 && blocks[i].firstrow <= blocks[i-1].firstrow {
				return 0, 0, errors.New("coldfmt: block first rows not increasing")
			}
			if blocks[i].firstrow >= s.NRows {
				return 0, 0, errors.New("coldfmt: block first row past row count")
			}
		}
		s.index[col] = blocks
	}
	if len(f)-pos < 12+4+4 {
		return 0, 0, errors.New("coldfmt: footer truncated after colindex")
	}
	dictOff = binary.LittleEndian.Uint64(f[pos:])
	dictLen = binary.LittleEndian.Uint32(f[pos+8:])
	pos += 12
	bloomLen := binary.LittleEndian.Uint32(f[len(f)-8:])
	if uint64(bloomLen) != uint64(len(f)-pos-8) || bloomLen%64 != 0 {
		return 0, 0, errors.New("coldfmt: bloom length mismatch")
	}
	s.bloom = f[pos : pos+int(bloomLen)]
	return dictOff, dictLen, nil
}

// MayContain is the urlfp bloom check: false means definitely absent.
func (s *PageSegment) MayContain(fp [16]byte) bool { return bloomTest(s.bloom, fp) }

// Close releases the segment's zstd state.
func (s *PageSegment) Close() { s.codec.close() }

// column decodes one column's blocks and returns the raw bytes per block,
// paired with each block's row count.
func (s *PageSegment) column(col int) (raws [][]byte, counts []int, err error) {
	blocks := s.index[col]
	for i, b := range blocks {
		if b.offset > uint64(len(s.data)) ||
			uint64(b.storedlen) > uint64(len(s.data))-b.offset-blockHeaderSize-4 {
			return nil, nil, errors.New("coldfmt: block extent out of bounds")
		}
		raw, err := s.codec.decodeBlock(s.data[b.offset : b.offset+blockHeaderSize+uint64(b.storedlen)+4])
		if err != nil {
			return nil, nil, err
		}
		if uint32(len(raw)) != b.rawlen {
			return nil, nil, errors.New("coldfmt: block rawlen disagrees with colindex")
		}
		next := s.NRows
		if i+1 < len(blocks) {
			next = blocks[i+1].firstrow
		}
		raws = append(raws, raw)
		counts = append(counts, int(next-b.firstrow))
	}
	return raws, counts, nil
}

// Rows decodes every column back into rows; the round-trip proof and the
// small-corpus read path. Bulk build stages will read single columns with
// ranged GETs instead.
func (s *PageSegment) Rows() ([]PageRow, error) {
	rows := make([]PageRow, 0, s.NRows)
	// Column 0 sizes the row slice; every later column must agree exactly.
	raws, counts, err := s.column(0)
	if err != nil {
		return nil, err
	}
	for bi, raw := range raws {
		if len(raw) != counts[bi]*16 {
			return nil, errors.New("coldfmt: urlfp column size mismatch")
		}
		for i := range counts[bi] {
			var r PageRow
			copy(r.URLFP[:], raw[i*16:])
			rows = append(rows, r)
		}
	}
	if uint64(len(rows)) != s.NRows {
		return nil, errors.New("coldfmt: row count mismatch")
	}
	for col := 1; col < pageNCols; col++ {
		if err := s.decodeColumn(col, rows); err != nil {
			return nil, fmt.Errorf("column %d: %w", col, err)
		}
	}
	return rows, nil
}

func (s *PageSegment) decodeColumn(col int, rows []PageRow) error {
	raws, counts, err := s.column(col)
	if err != nil {
		return err
	}
	row := 0
	for bi, raw := range raws {
		pos := 0
		for i := range counts[bi] {
			n, err := decodePageCell(raw[pos:], col, &rows[row], i == 0, row, rows)
			if err != nil {
				return err
			}
			pos += n
			row++
		}
		if pos != len(raw) {
			return errors.New("coldfmt: trailing bytes in column block")
		}
	}
	if row != len(rows) {
		return errors.New("coldfmt: cell count mismatch")
	}
	return nil
}

// decodePageCell is the inverse of appendPageCell for columns 1..14.
func decodePageCell(data []byte, col int, r *PageRow, blockStart bool, row int, rows []PageRow) (int, error) {
	need := func(n int) error {
		if len(data) < n {
			return errors.New("coldfmt: cell truncated")
		}
		return nil
	}
	switch col {
	case 1:
		s, n, err := DecodeString(data)
		if err != nil {
			return 0, err
		}
		r.URL = s
		return n, nil
	case 2:
		v, n, err := Uvarint(data)
		if err != nil {
			return 0, err
		}
		if blockStart {
			r.FetchMS = v
			return n, nil
		}
		prev := rows[row-1].FetchMS
		if v > ^uint64(0)-prev {
			return 0, errors.New("coldfmt: fetch_ms overflow")
		}
		r.FetchMS = prev + v
		return n, nil
	case 3:
		if err := need(2); err != nil {
			return 0, err
		}
		r.Status = binary.LittleEndian.Uint16(data)
		return 2, nil
	case 4:
		if err := need(4); err != nil {
			return 0, err
		}
		r.Flags = binary.LittleEndian.Uint32(data)
		return 4, nil
	case 5:
		if err := need(32); err != nil {
			return 0, err
		}
		copy(r.SHA256[:], data)
		return 32, nil
	case 6:
		if err := need(8); err != nil {
			return 0, err
		}
		r.Simhash = binary.LittleEndian.Uint64(data)
		return 8, nil
	case 7:
		if err := need(1); err != nil {
			return 0, err
		}
		r.Lang = data[0]
		return 1, nil
	case 8:
		if err := need(16); err != nil {
			return 0, err
		}
		copy(r.CanonFP[:], data)
		return 16, nil
	case 9:
		b, n, err := DecodeBytes(data)
		if err != nil {
			return 0, err
		}
		if len(b) > 0 {
			r.Hdr = b
		}
		return n, nil
	case 10:
		s, n, err := DecodeString(data)
		if err != nil {
			return 0, err
		}
		r.Title = s
		return n, nil
	case 11:
		s, n, err := DecodeString(data)
		if err != nil {
			return 0, err
		}
		r.Text = s
		return n, nil
	case 12:
		count, pos, err := Uvarint(data)
		if err != nil {
			return 0, err
		}
		if count > 65535 {
			return 0, errors.New("coldfmt: outlink count exceeds u16")
		}
		for range count {
			if len(data)-pos < 17 {
				return 0, errors.New("coldfmt: outlink truncated")
			}
			var l Outlink
			copy(l.Dst[:], data[pos:])
			l.Flags = data[pos+16]
			pos += 17
			a, n, err := DecodeString(data[pos:])
			if err != nil {
				return 0, err
			}
			if len(a) > MaxAnchorLen {
				return 0, errors.New("coldfmt: anchor exceeds store-time cap")
			}
			l.Anchor = a
			pos += n
			r.Outlinks = append(r.Outlinks, l)
		}
		return pos, nil
	case 13:
		if err := need(16); err != nil {
			return 0, err
		}
		r.RawRef.Seq = binary.LittleEndian.Uint64(data)
		r.RawRef.Off = binary.LittleEndian.Uint32(data[8:])
		r.RawRef.Len = binary.LittleEndian.Uint32(data[12:])
		return 16, nil
	default: // 14
		if err := need(2); err != nil {
			return 0, err
		}
		r.LawVer = binary.LittleEndian.Uint16(data)
		return 2, nil
	}
}
