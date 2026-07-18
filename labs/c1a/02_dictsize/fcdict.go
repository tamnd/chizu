package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// The front-coded dictionary writer and reader are copies of hotfmt's
// (dict.go) with the block size parameterized where production fixes
// 64. The test file pins the copies byte-for-byte against hotfmt at
// block 64 so the sweep cannot drift from what it claims to measure.

const (
	labEntrySize   = 32
	labMaxTermLen  = 64
	labTrailerSize = 20
	labInlined     = 1 << 0
	labMaxMask     = 0x0F
)

// labEntry mirrors hotfmt.DictEntry, inline postings included, so the
// lookup arm parses the same entry mix production would.
type labEntry struct {
	DF          uint32
	CF          uint64
	PostingsOff uint64
	PostingsLen uint32
	SkipOff     uint64
	Inline      []labInlinePosting
}

type labInlinePosting struct {
	Docid uint32
	TF    uint8
	Mask  uint8
}

func appendLabU48(dst []byte, v uint64) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40))
}

func labU48(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 | uint64(b[4])<<32 | uint64(b[5])<<40
}

func appendLabEntry(dst []byte, e *labEntry) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, e.DF)
	if len(e.Inline) > 0 {
		for _, p := range e.Inline {
			dst = binary.LittleEndian.AppendUint32(dst, p.Docid)
			dst = append(dst, p.TF, p.Mask)
		}
		dst = append(dst, make([]byte, 24-6*len(e.Inline))...)
		return append(dst, labInlined, 0, 0, 0)
	}
	dst = appendLabU48(dst, e.CF)
	dst = appendLabU48(dst, e.PostingsOff)
	dst = binary.LittleEndian.AppendUint32(dst, e.PostingsLen)
	dst = appendLabU48(dst, e.SkipOff)
	return append(dst, 0, 0, 0, 0, 0, 0)
}

func parseLabEntry(b []byte) (labEntry, error) {
	e := labEntry{DF: binary.LittleEndian.Uint32(b)}
	flags := b[28]
	if flags&^byte(labInlined) != 0 || b[29] != 0 || b[30] != 0 || b[31] != 0 {
		return e, errors.New("entry padding not zero")
	}
	if flags&labInlined != 0 {
		if e.DF == 0 || e.DF > 4 {
			return e, fmt.Errorf("inlined entry with df %d", e.DF)
		}
		e.Inline = make([]labInlinePosting, e.DF)
		for i := range e.Inline {
			e.Inline[i] = labInlinePosting{
				Docid: binary.LittleEndian.Uint32(b[4+6*i:]),
				TF:    b[8+6*i],
				Mask:  b[9+6*i],
			}
		}
		for _, spare := range b[4+6*e.DF : 28] {
			if spare != 0 {
				return e, errors.New("inline spare bytes not zero")
			}
		}
	} else {
		e.CF = labU48(b[4:])
		e.PostingsOff = labU48(b[10:])
		e.PostingsLen = binary.LittleEndian.Uint32(b[16:])
		e.SkipOff = labU48(b[20:])
		if b[26] != 0 || b[27] != 0 {
			return e, errors.New("entry spare bytes not zero")
		}
	}
	return e, nil
}

// labDictWriter is hotfmt.DictWriter with the block size a field, plus
// byte counters for the size arm.
type labDictWriter struct {
	blockSize int

	band    []byte
	index   []byte
	prev    []byte
	nterms  uint64
	nblocks uint32

	blockTerms   []byte
	blockEntries []byte
	blockCount   int

	termBytes  int // front-coded term bytes across all blocks
	entryBytes int // fixed entry bytes across all blocks
}

func (w *labDictWriter) add(term []byte, e *labEntry) error {
	if len(term) == 0 || len(term) > labMaxTermLen {
		return fmt.Errorf("term length %d", len(term))
	}
	if w.nterms > 0 && bytes.Compare(term, w.prev) <= 0 {
		return errors.New("terms not strictly increasing")
	}
	if w.blockCount == 0 {
		w.index = appendLabU48(w.index, uint64(len(w.band)))
		w.index = append(w.index, byte(len(term)))
		w.index = append(w.index, term...)
		w.blockTerms = append(w.blockTerms, byte(len(term)))
		w.blockTerms = append(w.blockTerms, term...)
	} else {
		lcp := 0
		for lcp < len(term) && lcp < len(w.prev) && term[lcp] == w.prev[lcp] {
			lcp++
		}
		w.blockTerms = append(w.blockTerms, byte(lcp), byte(len(term)-lcp))
		w.blockTerms = append(w.blockTerms, term[lcp:]...)
	}
	w.blockEntries = appendLabEntry(w.blockEntries, e)
	w.prev = append(w.prev[:0], term...)
	w.nterms++
	w.blockCount++
	if w.blockCount == w.blockSize {
		w.flushBlock()
	}
	return nil
}

func (w *labDictWriter) flushBlock() {
	w.termBytes += len(w.blockTerms)
	w.entryBytes += len(w.blockEntries)
	w.band = append(w.band, w.blockTerms...)
	w.band = append(w.band, w.blockEntries...)
	w.blockTerms = w.blockTerms[:0]
	w.blockEntries = w.blockEntries[:0]
	w.blockCount = 0
	w.nblocks++
}

func (w *labDictWriter) seal() ([]byte, error) {
	if w.nterms == 0 {
		return nil, errors.New("empty dictionary")
	}
	if w.blockCount > 0 {
		w.flushBlock()
	}
	indexOff := uint64(len(w.band))
	out := append(w.band, w.index...)
	out = binary.LittleEndian.AppendUint64(out, indexOff)
	out = binary.LittleEndian.AppendUint32(out, w.nblocks)
	return binary.LittleEndian.AppendUint64(out, w.nterms), nil
}

// indexBytes is the RAM-resident block index cost: index area plus
// trailer.
func (w *labDictWriter) indexBytes() int { return len(w.index) + labTrailerSize }

// labDict mirrors hotfmt.Dict: parsed block index, blocks scanned per
// lookup, block size parameterized.
type labDict struct {
	blockSize int
	data      []byte
	nterms    uint64
	blockOff  []uint64
	first     [][]byte
	indexOff  uint64
}

func openLabDict(data []byte, blockSize int) (*labDict, error) {
	if len(data) < labTrailerSize+labEntrySize {
		return nil, errors.New("dictionary band too short")
	}
	t := data[len(data)-labTrailerSize:]
	d := &labDict{
		blockSize: blockSize,
		data:      data,
		indexOff:  binary.LittleEndian.Uint64(t),
		nterms:    binary.LittleEndian.Uint64(t[12:]),
	}
	nblocks := binary.LittleEndian.Uint32(t[8:])
	if d.nterms == 0 || nblocks == 0 {
		return nil, errors.New("empty dictionary")
	}
	if uint64(nblocks) != (d.nterms+uint64(blockSize)-1)/uint64(blockSize) {
		return nil, errors.New("block count disagrees with term count")
	}
	if d.indexOff >= uint64(len(data)-labTrailerSize) {
		return nil, errors.New("dictionary index outside band")
	}
	idx := data[d.indexOff : len(data)-labTrailerSize]
	d.blockOff = make([]uint64, 0, nblocks)
	d.first = make([][]byte, 0, nblocks)
	for range nblocks {
		if len(idx) < 8 {
			return nil, errors.New("dictionary index truncated")
		}
		off := labU48(idx)
		l := int(idx[6])
		if l == 0 || l > labMaxTermLen || len(idx) < 7+l {
			return nil, errors.New("dictionary index term malformed")
		}
		d.blockOff = append(d.blockOff, off)
		d.first = append(d.first, idx[7:7+l])
		idx = idx[7+l:]
	}
	if len(idx) != 0 {
		return nil, errors.New("dictionary index has trailing bytes")
	}
	return d, nil
}

// lookup is hotfmt's Dict.Lookup shape: binary search the index, scan
// one block, allocation behavior kept so the lab pays what production
// pays.
func (d *labDict) lookup(term []byte) (labEntry, bool, error) {
	b := sort.Search(len(d.blockOff), func(i int) bool {
		return bytes.Compare(d.first[i], term) > 0
	}) - 1
	if b < 0 {
		return labEntry{}, false, nil
	}
	var found labEntry
	var ok bool
	err := d.scanBlock(b, func(t []byte, e labEntry) {
		if bytes.Equal(t, term) {
			found, ok = e, true
		}
	})
	return found, ok, err
}

func (d *labDict) scanBlock(b int, fn func(term []byte, e labEntry)) error {
	n := d.blockSize
	if b == len(d.blockOff)-1 {
		n = int(d.nterms - uint64(b)*uint64(d.blockSize))
	}
	area := d.data[d.blockOff[b]:d.indexOff]
	var term []byte
	pos := 0
	if pos >= len(area) {
		return errors.New("dictionary block truncated")
	}
	l := int(area[pos])
	if l == 0 || l > labMaxTermLen || pos+1+l > len(area) {
		return errors.New("dictionary term malformed")
	}
	term = append(term, area[pos+1:pos+1+l]...)
	if !bytes.Equal(term, d.first[b]) {
		return errors.New("block first term disagrees with index")
	}
	pos += 1 + l
	terms := make([][]byte, 1, n)
	terms[0] = bytes.Clone(term)
	for range n - 1 {
		if pos+2 > len(area) {
			return errors.New("dictionary block truncated")
		}
		lcp, sl := int(area[pos]), int(area[pos+1])
		pos += 2
		if lcp > len(term) || sl == 0 || lcp+sl > labMaxTermLen || pos+sl > len(area) {
			return errors.New("dictionary term malformed")
		}
		suffix := area[pos : pos+sl]
		pos += sl
		if lcp < len(term) && suffix[0] <= term[lcp] {
			return errors.New("dictionary term not front-coded canonically")
		}
		term = append(term[:lcp], suffix...)
		terms = append(terms, bytes.Clone(term))
	}
	if pos+n*labEntrySize > len(area) {
		return errors.New("dictionary entries truncated")
	}
	for i := range n {
		e, err := parseLabEntry(area[pos+i*labEntrySize:])
		if err != nil {
			return err
		}
		fn(terms[i], e)
	}
	return nil
}
