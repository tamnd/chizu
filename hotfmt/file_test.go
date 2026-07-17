package hotfmt

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"reflect"
	"testing"
)

func fixtureHeader() *FileHeader {
	return &FileHeader{
		Shard: 7, Generation: 12, Kind: KindBase,
		DocCount: 4321, TermCount: 99_000, TokenizerVer: 2,
		QuantScale: 0.125, Writer: 3,
	}
}

func fixtureMeta() *Meta {
	return &Meta{
		LawVer: 1, TokenizerVer: 2, QuantScale: 0.125, QuantPolicy: 1,
		DocCount: 4321,
		Fields: []MetaField{
			{ID: 0, Name: "body", SumLen: 5_000_000},
			{ID: 1, Name: "title", SumLen: 40_000},
			{ID: 2, Name: "anchor", SumLen: 90_000},
			{ID: 3, Name: "url", SumLen: 30_000},
		},
		Lineage: []uint64{12},
	}
}

func fixtureStats() *FieldStats {
	return &FieldStats{
		K1: 1.2, Alpha: 1.0, Beta: 1.0,
		Fields: []FieldStat{
			{ID: 0, TotalTokens: 5_000_000, AvgLen: 1157.1, Weight: 1.0, B: 0.75},
			{ID: 1, TotalTokens: 40_000, AvgLen: 9.3, Weight: 2.5, B: 0.6},
			{ID: 2, TotalTokens: 90_000, AvgLen: 20.8, Weight: 2.0, B: 0.5},
			{ID: 3, TotalTokens: 30_000, AvgLen: 6.9, Weight: 1.5, B: 0.4},
		},
	}
}

func fixtureBands() map[byte][]byte {
	rng := rand.New(rand.NewSource(2107))
	band := func(n int) []byte {
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	return map[byte][]byte{
		BandDict:      band(10_000),
		BandPostings:  band(50_000),
		BandSkips:     band(4_096), // exactly one aligned page
		BandPositions: band(60_001),
		BandDocvalues: band(3_000),
		BandDocband:   band(20_000),
		// tombstones absent: a fresh base has none
	}
}

func fixtureProv() *Provenance {
	return &Provenance{
		Builder: 3, BuildMS: 1_750_000_000_000,
		Watermarks: []uint64{500, 480, 510, 505, 490},
		Stats:      []byte("stats block placeholder"),
	}
}

func fixtureFile(t testing.TB) []byte {
	t.Helper()
	data, err := EncodeFile(fixtureHeader(), fixtureMeta(), fixtureStats(), fixtureBands(), fixtureProv())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func openBytes(data []byte) (*Shard, error) {
	return Open(bytes.NewReader(data), int64(len(data)))
}

func TestFileRoundTrip(t *testing.T) {
	data := fixtureFile(t)
	s, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Header, *fixtureHeader()) {
		t.Fatalf("header: %+v", s.Header)
	}
	if !reflect.DeepEqual(s.Meta, fixtureMeta()) {
		t.Fatalf("meta: %+v", s.Meta)
	}
	if !reflect.DeepEqual(s.Stats, fixtureStats()) {
		t.Fatalf("stats: %+v", s.Stats)
	}
	if !reflect.DeepEqual(&s.Provenance, fixtureProv()) {
		t.Fatalf("provenance: %+v", s.Provenance)
	}
	bands := fixtureBands()
	for id, want := range bands {
		got, err := s.ReadBand(id)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("band %d differs", id)
		}
	}
	if got, err := s.ReadBand(BandTombstones); err != nil || len(got) != 0 {
		t.Fatalf("empty tombstones band: %v %d bytes", err, len(got))
	}
	if err := s.VerifyBands(); err != nil {
		t.Fatal(err)
	}
	for _, id := range bandOrder {
		off, _, err := s.Band(id)
		if err != nil {
			t.Fatal(err)
		}
		if off%align != 0 {
			t.Fatalf("band %d at unaligned offset %d", id, off)
		}
	}
	if _, _, err := s.Band(42); err == nil {
		t.Fatal("unknown band id answered")
	}
}

func TestEncodeFileRejectsBadInput(t *testing.T) {
	h, m, st, b, p := fixtureHeader(), fixtureMeta(), fixtureStats(), fixtureBands(), fixtureProv()

	h.Kind = 0
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("kind 0 accepted")
	}
	h.Kind = KindBase
	h.BaseGen = 5
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("base with base_gen accepted")
	}
	h.BaseGen = 0

	h.DocCount++
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("doc count skew accepted")
	}
	h.DocCount--

	m.QuantScale = 0.5
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("quant scale skew accepted")
	}
	m.QuantScale = h.QuantScale

	st.Fields = st.Fields[:3]
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("field schema skew accepted")
	}
	st.Fields = fixtureStats().Fields

	b[BandMeta] = []byte("x")
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("caller-supplied meta band accepted")
	}
	delete(b, BandMeta)
	b[42] = []byte("x")
	if _, err := EncodeFile(h, m, st, b, p); err == nil {
		t.Fatal("unknown band id accepted")
	}
	delete(b, 42)

	if _, err := EncodeFile(h, m, st, b, p); err != nil {
		t.Fatalf("fixture no longer encodes: %v", err)
	}
}

func TestOpenRejectsDeltaSkew(t *testing.T) {
	h, m := fixtureHeader(), fixtureMeta()
	h.Kind, h.BaseGen = KindDelta, 12
	h.Generation = 13
	m.Lineage = []uint64{12, 13}
	data, err := EncodeFile(h, m, fixtureStats(), fixtureBands(), fixtureProv())
	if err != nil {
		t.Fatal(err)
	}
	s, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Header.Kind != KindDelta || s.Header.BaseGen != 12 {
		t.Fatalf("delta header: %+v", s.Header)
	}

	h.BaseGen = 13
	if _, err := EncodeFile(h, m, fixtureStats(), fixtureBands(), fixtureProv()); err == nil {
		t.Fatal("delta with base_gen == generation accepted")
	}
}

func TestOpenRejectsTruncation(t *testing.T) {
	data := fixtureFile(t)
	for _, cut := range []int{1, tailSize, tailSize + 3, len(data) - align} {
		if _, err := openBytes(data[:len(data)-cut]); err == nil {
			t.Fatalf("file truncated by %d accepted", cut)
		}
	}
}

// paddingByte reports whether byte i lies in alignment padding, the
// only crc-less bytes in the file: they are never handed to any reader,
// so a flip there must leave the decode identical.
func paddingByte(s *Shard, i int, footerOff uint64) bool {
	off := uint64(i)
	if off >= uint64(hotHeaderSize) && off < align {
		return true
	}
	for _, e := range s.bands {
		if off >= e.off+e.len && off < alignUp(e.off+e.len) && alignUp(e.off+e.len) <= footerOff {
			return true
		}
	}
	return false
}

func TestFileFlipSweep(t *testing.T) {
	data := fixtureFile(t)
	want, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	footerOff := binary.LittleEndian.Uint64(data[len(data)-tailSize:])
	for i := 0; i < len(data); i += 7 {
		bad := bytes.Clone(data)
		bad[i] ^= 0x20
		got, err := openBytes(bad)
		if err != nil {
			continue
		}
		if verr := got.VerifyBands(); verr != nil {
			continue
		}
		writerField := i >= 24 && i < 32
		if !writerField && !paddingByte(want, i, footerOff) {
			t.Fatalf("flip at %d accepted silently", i)
		}
		got.Header.Writer = want.Header.Writer
		if !reflect.DeepEqual(got.Header, want.Header) ||
			!reflect.DeepEqual(got.Meta, want.Meta) ||
			!reflect.DeepEqual(got.Stats, want.Stats) ||
			!reflect.DeepEqual(got.Provenance, want.Provenance) {
			t.Fatalf("flip at %d changed the decode without an error", i)
		}
	}
}

func FuzzOpenFile(f *testing.F) {
	full, err := EncodeFile(fixtureHeader(), fixtureMeta(), fixtureStats(), fixtureBands(), fixtureProv())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(full)
	small, err := EncodeFile(fixtureHeader(), fixtureMeta(), fixtureStats(), nil, &Provenance{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(small)
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := openBytes(data)
		if err != nil {
			return
		}
		// Every accepted file must re-encode, and the result must equal
		// the input on every non-padding byte: padding is dead by
		// construction, everything else is either re-emitted or rejected.
		bands := map[byte][]byte{}
		for _, id := range bandOrder {
			if id == BandMeta || id == BandFieldstats {
				continue
			}
			payload, err := s.ReadBand(id)
			if err != nil {
				return
			}
			bands[id] = payload
		}
		out, err := EncodeFile(&s.Header, s.Meta, s.Stats, bands, &s.Provenance)
		if err != nil {
			t.Fatalf("accepted file re-encode failed: %v", err)
		}
		if len(out) != len(data) {
			t.Fatalf("re-encode length %d != input %d", len(out), len(data))
		}
		footerOff := binary.LittleEndian.Uint64(data[len(data)-tailSize:])
		for i := range data {
			if data[i] != out[i] && !paddingByte(s, i, footerOff) {
				t.Fatalf("non-padding byte %d differs after re-encode", i)
			}
		}
	})
}
