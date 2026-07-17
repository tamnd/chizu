package hotfmt

import (
	"bytes"
	"reflect"
	"testing"
)

func TestTombstonesRoundTripSparse(t *testing.T) {
	in := &Tombstones{
		BaseDocCount: 100000,
		Watermark:    777,
		Docids:       []uint32{0, 5, 6, 99999},
	}
	band, err := EncodeTombstones(in)
	if err != nil {
		t.Fatal(err)
	}
	if band[0] != tombSparse {
		t.Fatalf("kind %d, want sparse: 4 docids in a 100k base", band[0])
	}
	got, err := ParseTombstones(band)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip drifted: %+v", got)
	}
}

func TestTombstonesRoundTripDense(t *testing.T) {
	in := &Tombstones{BaseDocCount: 1000, Watermark: 3}
	for i := uint32(0); i < 1000; i += 2 {
		in.Docids = append(in.Docids, i)
	}
	band, err := EncodeTombstones(in)
	if err != nil {
		t.Fatal(err)
	}
	if band[0] != tombDense {
		t.Fatalf("kind %d, want dense: 500 docids in a 1000-doc base", band[0])
	}
	got, err := ParseTombstones(band)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip drifted: %d docids out", len(got.Docids))
	}
}

func TestTombstonesRejects(t *testing.T) {
	if _, err := EncodeTombstones(&Tombstones{BaseDocCount: 0}); err == nil {
		t.Error("empty base accepted")
	}
	if _, err := EncodeTombstones(&Tombstones{BaseDocCount: 10, Docids: []uint32{10}}); err == nil {
		t.Error("docid past base accepted")
	}
	if _, err := EncodeTombstones(&Tombstones{BaseDocCount: 10, Docids: []uint32{3, 3}}); err == nil {
		t.Error("duplicate docid accepted")
	}

	band, err := EncodeTombstones(&Tombstones{BaseDocCount: 100000, Watermark: 1, Docids: []uint32{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTombstones(band[:10]); err == nil {
		t.Error("truncated band accepted")
	}
	bad := bytes.Clone(band)
	bad[0] = tombDense // sparse payload under dense kind
	if _, err := ParseTombstones(bad); err == nil {
		t.Error("non-canonical representation accepted")
	}
	bad = bytes.Clone(band)
	bad[0] = 9
	if _, err := ParseTombstones(bad); err == nil {
		t.Error("unknown kind accepted")
	}

	dense := &Tombstones{BaseDocCount: 100, Watermark: 1}
	for i := range uint32(90) {
		dense.Docids = append(dense.Docids, i)
	}
	dband, err := EncodeTombstones(dense)
	if err != nil {
		t.Fatal(err)
	}
	bad = bytes.Clone(dband)
	bad[len(bad)-1] |= 0xF0 // bits past base doc count (base 100, tail 4 bits)
	if _, err := ParseTombstones(bad); err == nil {
		t.Error("bits past base doc count accepted")
	}
	bad = bytes.Clone(dband)
	bad[tombHeaderSize] ^= 0x02 // flip a bit: popcount disagrees with count
	if _, err := ParseTombstones(bad); err == nil {
		t.Error("popcount skew accepted")
	}
}

func TestBuildExclusion(t *testing.T) {
	d1 := &Tombstones{BaseDocCount: 100, Watermark: 1, Docids: []uint32{1, 50}}
	d2 := &Tombstones{BaseDocCount: 100, Watermark: 2, Docids: []uint32{50, 99}}
	set, err := BuildExclusion(100, []*Tombstones{d1, d2})
	if err != nil {
		t.Fatal(err)
	}
	for docid, want := range map[uint32]bool{0: false, 1: true, 50: true, 99: true, 98: false} {
		if Excluded(set, docid) != want {
			t.Fatalf("Excluded(%d) = %v", docid, !want)
		}
	}
	if _, err := BuildExclusion(200, []*Tombstones{d1}); err == nil {
		t.Error("base mismatch accepted")
	}
}

func FuzzParseTombstones(f *testing.F) {
	sparse := &Tombstones{BaseDocCount: 100000, Watermark: 7, Docids: []uint32{3, 90000}}
	dense := &Tombstones{BaseDocCount: 64, Watermark: 7}
	for i := uint32(0); i < 64; i += 2 {
		dense.Docids = append(dense.Docids, i)
	}
	for _, in := range []*Tombstones{sparse, dense} {
		if band, err := EncodeTombstones(in); err == nil {
			f.Add(band)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ts, err := ParseTombstones(data)
		if err != nil {
			return
		}
		out, err := EncodeTombstones(ts)
		if err != nil {
			t.Fatalf("re-encode of accepted tombstones: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted tombstones band is not canonical")
		}
	})
}
