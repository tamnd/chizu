package hotfmt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Dictionary band, doc 05 section 4: front-coded blocks of 64 terms,
// each block ending in fixed 32-byte entries, plus a block index the
// server keeps in RAM. A lookup is one binary search over the index and
// one linear scan of one block, no per-term pointer chasing.

const (
	dictBlockTerms = 64
	dictEntrySize  = 32

	// maxTermLen is the doc 06 admission cap; longer byte strings never
	// reach the dictionary.
	maxTermLen = 64

	// dictInlined marks a df<=4 entry whose union area holds the whole
	// postings list inline, so the term touches no other band.
	dictInlined byte = 1 << 0

	// maxFieldMask is the four-field schema's full mask.
	maxFieldMask = 0x0F

	dictTrailerSize = 20
)

// InlinePosting is one posting stored inside a df<=4 dictionary entry.
type InlinePosting struct {
	Docid uint32
	TF    uint8
	Mask  uint8
}

// DictEntry is one term's fixed entry. For inlined terms the offsets
// are zero and Inline carries the postings; CF is then the sum of the
// inline tfs, recovered rather than stored.
type DictEntry struct {
	DF          uint32
	CF          uint64
	PostingsOff uint64
	PostingsLen uint32
	SkipOff     uint64
	Inline      []InlinePosting
}

func (e *DictEntry) inlined() bool { return len(e.Inline) > 0 }

func (e *DictEntry) validate() error {
	if e.DF == 0 {
		return errors.New("hotfmt: dictionary entry with df 0")
	}
	if e.DF <= 4 {
		if len(e.Inline) != int(e.DF) {
			return fmt.Errorf("hotfmt: df %d entry must inline exactly its postings", e.DF)
		}
		if e.CF != 0 || e.PostingsOff != 0 || e.PostingsLen != 0 || e.SkipOff != 0 {
			return errors.New("hotfmt: inlined entry with band offsets")
		}
		for i, p := range e.Inline {
			if i > 0 && p.Docid <= e.Inline[i-1].Docid {
				return errors.New("hotfmt: inline postings not strictly increasing")
			}
			if p.TF == 0 || p.Mask == 0 || p.Mask > maxFieldMask {
				return errors.New("hotfmt: bad inline posting")
			}
		}
		return nil
	}
	if len(e.Inline) != 0 {
		return fmt.Errorf("hotfmt: df %d entry cannot inline", e.DF)
	}
	if e.CF < uint64(e.DF) {
		return errors.New("hotfmt: cf below df")
	}
	if e.CF > maxU48 || e.PostingsOff > maxU48 || e.SkipOff > maxU48 {
		return errors.New("hotfmt: dictionary offset exceeds u48")
	}
	return nil
}

const maxU48 = 1<<48 - 1

func appendDictEntry(dst []byte, e *DictEntry) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, e.DF)
	if e.inlined() {
		for _, p := range e.Inline {
			dst = binary.LittleEndian.AppendUint32(dst, p.Docid)
			dst = append(dst, p.TF, p.Mask)
		}
		dst = append(dst, make([]byte, 24-6*len(e.Inline))...)
		return append(dst, dictInlined, 0, 0, 0)
	}
	dst = appendU48(dst, e.CF)
	dst = appendU48(dst, e.PostingsOff)
	dst = binary.LittleEndian.AppendUint32(dst, e.PostingsLen)
	dst = appendU48(dst, e.SkipOff)
	return append(dst, 0, 0, 0, 0, 0, 0)
}

func parseDictEntry(b []byte) (DictEntry, error) {
	e := DictEntry{DF: binary.LittleEndian.Uint32(b)}
	flags := b[28]
	if flags&^dictInlined != 0 || b[29] != 0 || b[30] != 0 || b[31] != 0 {
		return e, errors.New("hotfmt: dictionary entry padding not zero")
	}
	if flags&dictInlined != 0 {
		if e.DF == 0 || e.DF > 4 {
			return e, fmt.Errorf("hotfmt: inlined entry with df %d", e.DF)
		}
		e.Inline = make([]InlinePosting, e.DF)
		for i := range e.Inline {
			e.Inline[i] = InlinePosting{
				Docid: binary.LittleEndian.Uint32(b[4+6*i:]),
				TF:    b[8+6*i],
				Mask:  b[9+6*i],
			}
		}
		for _, spare := range b[4+6*e.DF : 28] {
			if spare != 0 {
				return e, errors.New("hotfmt: inline spare bytes not zero")
			}
		}
	} else {
		e.CF = u48(b[4:])
		e.PostingsOff = u48(b[10:])
		e.PostingsLen = binary.LittleEndian.Uint32(b[16:])
		e.SkipOff = u48(b[20:])
		if b[26] != 0 || b[27] != 0 {
			return e, errors.New("hotfmt: dictionary entry spare bytes not zero")
		}
	}
	if err := e.validate(); err != nil {
		return e, err
	}
	return e, nil
}

// DictWriter builds the dictionary band. Terms must arrive in strictly
// increasing byte order.
type DictWriter struct {
	band    []byte
	index   []byte
	prev    []byte
	nterms  uint64
	nblocks uint32

	blockTerms   []byte
	blockEntries []byte
	blockCount   int
}

// Add appends one term and its entry.
func (w *DictWriter) Add(term []byte, e *DictEntry) error {
	if len(term) == 0 || len(term) > maxTermLen {
		return fmt.Errorf("hotfmt: term length %d", len(term))
	}
	if w.nterms > 0 && bytes.Compare(term, w.prev) <= 0 {
		return errors.New("hotfmt: terms not strictly increasing")
	}
	if err := e.validate(); err != nil {
		return err
	}
	if w.blockCount == 0 {
		w.index = appendU48(w.index, uint64(len(w.band)))
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
	w.blockEntries = appendDictEntry(w.blockEntries, e)
	w.prev = append(w.prev[:0], term...)
	w.nterms++
	w.blockCount++
	if w.blockCount == dictBlockTerms {
		w.flushBlock()
	}
	return nil
}

func (w *DictWriter) flushBlock() {
	w.band = append(w.band, w.blockTerms...)
	w.band = append(w.band, w.blockEntries...)
	w.blockTerms = w.blockTerms[:0]
	w.blockEntries = w.blockEntries[:0]
	w.blockCount = 0
	w.nblocks++
}

// Seal finishes the band: blocks, then the index, then the trailer.
func (w *DictWriter) Seal() ([]byte, error) {
	if w.nterms == 0 {
		return nil, errors.New("hotfmt: empty dictionary")
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

// Dict is an opened dictionary band: the parsed block index plus the
// raw band, blocks scanned lazily per lookup.
type Dict struct {
	data     []byte
	nterms   uint64
	blockOff []uint64
	first    [][]byte
	indexOff uint64
}

// OpenDict parses the trailer and the block index.
func OpenDict(data []byte) (*Dict, error) {
	if len(data) < dictTrailerSize+dictEntrySize {
		return nil, errors.New("hotfmt: dictionary band too short")
	}
	t := data[len(data)-dictTrailerSize:]
	d := &Dict{
		data:     data,
		indexOff: binary.LittleEndian.Uint64(t),
		nterms:   binary.LittleEndian.Uint64(t[12:]),
	}
	nblocks := binary.LittleEndian.Uint32(t[8:])
	if d.nterms == 0 || nblocks == 0 {
		return nil, errors.New("hotfmt: empty dictionary")
	}
	if uint64(nblocks) != (d.nterms+dictBlockTerms-1)/dictBlockTerms {
		return nil, errors.New("hotfmt: block count disagrees with term count")
	}
	if d.indexOff >= uint64(len(data)-dictTrailerSize) {
		return nil, errors.New("hotfmt: dictionary index outside band")
	}
	idx := data[d.indexOff : len(data)-dictTrailerSize]
	d.blockOff = make([]uint64, 0, nblocks)
	d.first = make([][]byte, 0, nblocks)
	for range nblocks {
		if len(idx) < 8 {
			return nil, errors.New("hotfmt: dictionary index truncated")
		}
		off := u48(idx)
		l := int(idx[6])
		if l == 0 || l > maxTermLen || len(idx) < 7+l {
			return nil, errors.New("hotfmt: dictionary index term malformed")
		}
		term := idx[7 : 7+l]
		if n := len(d.blockOff); n > 0 {
			if off <= d.blockOff[n-1] || bytes.Compare(term, d.first[n-1]) <= 0 {
				return nil, errors.New("hotfmt: dictionary index not strictly increasing")
			}
		} else if off != 0 {
			return nil, errors.New("hotfmt: first dictionary block not at offset 0")
		}
		if off >= d.indexOff {
			return nil, errors.New("hotfmt: dictionary block outside blocks area")
		}
		d.blockOff = append(d.blockOff, off)
		d.first = append(d.first, term)
		idx = idx[7+l:]
	}
	if len(idx) != 0 {
		return nil, errors.New("hotfmt: dictionary index has trailing bytes")
	}
	return d, nil
}

// NTerms is the dictionary's term count.
func (d *Dict) NTerms() uint64 { return d.nterms }

// Lookup finds one term. The second return is false when the term is
// absent and every byte scanned parsed cleanly.
func (d *Dict) Lookup(term []byte) (DictEntry, bool, error) {
	b := sort.Search(len(d.blockOff), func(i int) bool {
		return bytes.Compare(d.first[i], term) > 0
	}) - 1
	if b < 0 {
		return DictEntry{}, false, nil
	}
	var found DictEntry
	var ok bool
	err := d.scanBlock(b, func(t []byte, e DictEntry) {
		if bytes.Equal(t, term) {
			found, ok = e, true
		}
	})
	return found, ok, err
}

// Walk visits every term in order; the merge path and the fuzz
// invariant both ride it. It additionally enforces that blocks are
// dense: each block must end exactly where the next begins.
func (d *Dict) Walk(fn func(term []byte, e DictEntry) error) error {
	for b := range d.blockOff {
		var werr error
		end, err := d.scanBlockEnd(b, func(t []byte, e DictEntry) {
			if werr == nil {
				werr = fn(t, e)
			}
		})
		if err != nil {
			return err
		}
		if werr != nil {
			return werr
		}
		next := d.indexOff
		if b+1 < len(d.blockOff) {
			next = d.blockOff[b+1]
		}
		if end != next {
			return errors.New("hotfmt: dictionary blocks not dense")
		}
	}
	return nil
}

func (d *Dict) scanBlock(b int, fn func(term []byte, e DictEntry)) error {
	_, err := d.scanBlockEnd(b, fn)
	return err
}

// scanBlockEnd decodes block b, calling fn per term, and returns the
// offset just past the block's entry array.
func (d *Dict) scanBlockEnd(b int, fn func(term []byte, e DictEntry)) (uint64, error) {
	n := dictBlockTerms
	if b == len(d.blockOff)-1 {
		n = int(d.nterms - uint64(b)*dictBlockTerms)
	}
	area := d.data[d.blockOff[b]:d.indexOff]
	var term []byte
	pos := 0
	// First term is stored full; the index carries the same bytes and
	// the two must agree.
	if pos >= len(area) {
		return 0, errors.New("hotfmt: dictionary block truncated")
	}
	l := int(area[pos])
	if l == 0 || l > maxTermLen || pos+1+l > len(area) {
		return 0, errors.New("hotfmt: dictionary term malformed")
	}
	term = append(term, area[pos+1:pos+1+l]...)
	if !bytes.Equal(term, d.first[b]) {
		return 0, errors.New("hotfmt: block first term disagrees with index")
	}
	pos += 1 + l
	terms := make([][]byte, 1, n)
	terms[0] = bytes.Clone(term)
	for range n - 1 {
		if pos+2 > len(area) {
			return 0, errors.New("hotfmt: dictionary block truncated")
		}
		lcp, sl := int(area[pos]), int(area[pos+1])
		pos += 2
		if lcp > len(term) || sl == 0 || lcp+sl > maxTermLen || pos+sl > len(area) {
			return 0, errors.New("hotfmt: dictionary term malformed")
		}
		suffix := area[pos : pos+sl]
		pos += sl
		// Strict order and maximal front-coding in one check: the first
		// suffix byte must exceed the byte it replaces, which also makes
		// this the only valid encoding of the term.
		if lcp < len(term) && suffix[0] <= term[lcp] {
			return 0, errors.New("hotfmt: dictionary term not front-coded canonically")
		}
		term = append(term[:lcp], suffix...)
		terms = append(terms, bytes.Clone(term))
	}
	if pos+n*dictEntrySize > len(area) {
		return 0, errors.New("hotfmt: dictionary entries truncated")
	}
	for i := range n {
		e, err := parseDictEntry(area[pos+i*dictEntrySize:])
		if err != nil {
			return 0, err
		}
		fn(terms[i], e)
	}
	return d.blockOff[b] + uint64(pos+n*dictEntrySize), nil
}
