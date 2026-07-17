package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Tombstones band, doc 05 section 10: base generations carry an empty
// band; a delta carries the base-local docids it deletes, as a sorted
// array when sparse or a bitset when dense, plus its docmap watermark.
// The representation is picked by arithmetic, not by the writer, so
// the encoding stays canonical.

const (
	tombSparse byte = 1
	tombDense  byte = 2

	tombHeaderSize = 17
)

// Tombstones is one delta's parsed band.
type Tombstones struct {
	BaseDocCount uint32
	Watermark    uint64
	Docids       []uint32 // sorted, strictly increasing, all < BaseDocCount
}

func tombDenseBytes(baseDocCount uint32) int {
	return int(baseDocCount+7) / 8
}

// tombKind picks the canonical representation: dense exactly when the
// bitset is smaller than the sorted array.
func tombKind(count int, baseDocCount uint32) byte {
	if count*4 > tombDenseBytes(baseDocCount) {
		return tombDense
	}
	return tombSparse
}

// EncodeTombstones encodes a delta's band. A base generation writes
// no band at all rather than an empty one.
func EncodeTombstones(t *Tombstones) ([]byte, error) {
	if t.BaseDocCount == 0 {
		return nil, errors.New("hotfmt: tombstones for empty base")
	}
	for i, d := range t.Docids {
		if d >= t.BaseDocCount {
			return nil, fmt.Errorf("hotfmt: tombstone docid %d past base doc count", d)
		}
		if i > 0 && d <= t.Docids[i-1] {
			return nil, errors.New("hotfmt: tombstone docids not strictly increasing")
		}
	}
	kind := tombKind(len(t.Docids), t.BaseDocCount)
	out := make([]byte, 0, tombHeaderSize+len(t.Docids)*4)
	out = append(out, kind)
	out = binary.LittleEndian.AppendUint32(out, t.BaseDocCount)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(t.Docids)))
	out = binary.LittleEndian.AppendUint64(out, t.Watermark)
	if kind == tombSparse {
		for _, d := range t.Docids {
			out = binary.LittleEndian.AppendUint32(out, d)
		}
		return out, nil
	}
	set := make([]byte, tombDenseBytes(t.BaseDocCount))
	for _, d := range t.Docids {
		set[d/8] |= 1 << (d % 8)
	}
	return append(out, set...), nil
}

// ParseTombstones decodes and validates a delta's band, rejecting the
// non-canonical representation for its density.
func ParseTombstones(data []byte) (*Tombstones, error) {
	if len(data) < tombHeaderSize {
		return nil, errors.New("hotfmt: tombstones band truncated")
	}
	kind := data[0]
	t := &Tombstones{
		BaseDocCount: binary.LittleEndian.Uint32(data[1:]),
		Watermark:    binary.LittleEndian.Uint64(data[9:]),
	}
	count := binary.LittleEndian.Uint32(data[5:])
	if t.BaseDocCount == 0 || count > t.BaseDocCount {
		return nil, errors.New("hotfmt: tombstone count past base doc count")
	}
	if kind != tombKind(int(count), t.BaseDocCount) {
		return nil, errors.New("hotfmt: non-canonical tombstone representation")
	}
	body := data[tombHeaderSize:]
	switch kind {
	case tombSparse:
		if len(body) != int(count)*4 {
			return nil, errors.New("hotfmt: tombstone array length disagrees with count")
		}
		t.Docids = make([]uint32, count)
		for i := range t.Docids {
			t.Docids[i] = binary.LittleEndian.Uint32(body[i*4:])
			if t.Docids[i] >= t.BaseDocCount {
				return nil, errors.New("hotfmt: tombstone docid past base doc count")
			}
			if i > 0 && t.Docids[i] <= t.Docids[i-1] {
				return nil, errors.New("hotfmt: tombstone docids not strictly increasing")
			}
		}
	case tombDense:
		if len(body) != tombDenseBytes(t.BaseDocCount) {
			return nil, errors.New("hotfmt: tombstone bitset length disagrees with base doc count")
		}
		n := 0
		for _, b := range body {
			n += bits.OnesCount8(b)
		}
		if n != int(count) {
			return nil, errors.New("hotfmt: tombstone bitset popcount disagrees with count")
		}
		if tail := t.BaseDocCount % 8; tail != 0 && body[len(body)-1]>>tail != 0 {
			return nil, errors.New("hotfmt: tombstone bits past base doc count")
		}
		t.Docids = make([]uint32, 0, count)
		for i, b := range body {
			for b != 0 {
				t.Docids = append(t.Docids, uint32(i*8+bits.TrailingZeros8(b)))
				b &= b - 1
			}
		}
	default:
		return nil, fmt.Errorf("hotfmt: unknown tombstone kind %d", kind)
	}
	return t, nil
}

// BuildExclusion merges the tombstone bands of every live delta over
// one base into the mount-time exclusion bitset the phase-1 loop
// tests, one bit per base-local docid.
func BuildExclusion(baseDocCount uint32, deltas []*Tombstones) ([]byte, error) {
	set := make([]byte, tombDenseBytes(baseDocCount))
	for _, t := range deltas {
		if t.BaseDocCount != baseDocCount {
			return nil, errors.New("hotfmt: delta tombstones for a different base")
		}
		for _, d := range t.Docids {
			set[d/8] |= 1 << (d % 8)
		}
	}
	return set, nil
}

// Excluded is the phase-1 inner-loop bit test.
func Excluded(set []byte, docid uint32) bool {
	return set[docid/8]&(1<<(docid%8)) != 0
}
