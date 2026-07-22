package serve

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/tamnd/chizu/hotfmt"
)

const testDocCount = 6000

// buildTestShard writes a small but complete .hot file: one head term
// over the L1 lock threshold, one torso term, one inlined term.
func buildTestShard(t *testing.T) string {
	t.Helper()
	var postingsBand, skipsBand, positionsBand []byte
	dict := map[string]*hotfmt.DictEntry{}

	addTerm := func(name string, df int) {
		ps := make([]hotfmt.Posting, df)
		for i := range ps {
			ps[i] = hotfmt.Posting{
				Docid:  uint32(i * (testDocCount - 1) / df),
				TF:     uint8(i%7 + 1),
				Mask:   1,
				Impact: uint8(i%90 + 1),
			}
		}
		enc, l1, err := hotfmt.EncodePostings(ps)
		if err != nil {
			t.Fatalf("%s postings: %v", name, err)
		}
		var cf uint64
		for _, p := range ps {
			cf += uint64(p.TF)
		}
		posOffs := make([]uint64, len(l1))
		for bi := range l1 {
			posOffs[bi] = uint64(len(positionsBand))
			positionsBand = append(positionsBand, 0) // placeholder run bytes
		}
		dict[name] = &hotfmt.DictEntry{
			DF:          uint32(df),
			CF:          cf,
			PostingsOff: uint64(len(postingsBand)),
			PostingsLen: uint32(len(enc)),
			SkipOff:     uint64(len(skipsBand)),
		}
		postingsBand = append(postingsBand, enc...)
		skipsBand, err = hotfmt.EncodeSkips(skipsBand, l1, posOffs)
		if err != nil {
			t.Fatalf("%s skips: %v", name, err)
		}
	}
	addTerm("aaa", 4500) // above the default head-L1 df floor
	addTerm("bbb", 200)
	dict["zzz"] = &hotfmt.DictEntry{
		DF:     2,
		Inline: []hotfmt.InlinePosting{{Docid: 3, TF: 1, Mask: 1}, {Docid: 9, TF: 2, Mask: 1}},
	}

	var dw hotfmt.DictWriter
	for _, name := range []string{"aaa", "bbb", "zzz"} {
		if err := dw.Add([]byte(name), dict[name]); err != nil {
			t.Fatalf("dict %s: %v", name, err)
		}
	}
	dictBand, err := dw.Seal()
	if err != nil {
		t.Fatal(err)
	}

	dvs := make([]hotfmt.DocValue, testDocCount)
	for i := range dvs {
		dvs[i] = hotfmt.DocValue{Quality: uint8(255 - i%256), Lang: 1, DoclenBody: 7}
	}
	dvBand, err := hotfmt.EncodeDocValues(dvs)
	if err != nil {
		t.Fatal(err)
	}

	dbw := hotfmt.NewDocBandWriter(t.TempDir())
	for i := range testDocCount {
		rec := hotfmt.DocRecord{
			URL:     fmt.Appendf(nil, "https://example%03d.test/doc-%d", i%50, i),
			Title:   fmt.Appendf(nil, "Doc %d", i),
			Snippet: fmt.Appendf(nil, "Snippet source for document %d, long enough to be real text.", i),
		}
		if err := dbw.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	var docBand bytes.Buffer
	if err := dbw.Seal(&docBand); err != nil {
		t.Fatal(err)
	}

	h := &hotfmt.FileHeader{
		Shard: 7, Generation: 12, Kind: hotfmt.KindBase,
		DocCount: testDocCount, TermCount: 3, TokenizerVer: 2,
		QuantScale: 0.125, Writer: 3,
	}
	meta := &hotfmt.Meta{
		LawVer: 1, TokenizerVer: 2, QuantScale: 0.125, QuantPolicy: 1,
		DocCount: testDocCount,
		Fields: []hotfmt.MetaField{
			{ID: 0, Name: "body", SumLen: 1_000_000},
			{ID: 1, Name: "title", SumLen: 20_000},
		},
		Lineage: []uint64{12},
	}
	stats := &hotfmt.FieldStats{
		K1: 1.2, Alpha: 1.0, Beta: 1.0,
		Fields: []hotfmt.FieldStat{
			{ID: 0, TotalTokens: 1_000_000, AvgLen: 166.7, Weight: 1.0, B: 0.75},
			{ID: 1, TotalTokens: 20_000, AvgLen: 3.3, Weight: 2.5, B: 0.6},
		},
	}
	prov := &hotfmt.Provenance{Builder: 3, BuildMS: 1, Watermarks: []uint64{1}, Stats: []byte("s")}
	data, err := hotfmt.EncodeFile(h, meta, stats, map[byte][]byte{
		hotfmt.BandDict:      dictBand,
		hotfmt.BandPostings:  postingsBand,
		hotfmt.BandSkips:     skipsBand,
		hotfmt.BandPositions: positionsBand,
		hotfmt.BandDocvalues: dvBand,
		hotfmt.BandDocband:   docBand.Bytes(),
	}, prov)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shard.hot")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mountForTest(t *testing.T, opts Options) *Mount {
	t.Helper()
	m, err := MountShard(buildTestShard(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !m.Closed() {
			_ = m.Close()
		}
	})
	return m
}

func TestMountResidentBands(t *testing.T) {
	m := mountForTest(t, Options{Mlock: MlockOff})

	e, ok, err := m.Dict.Lookup([]byte("aaa"))
	if err != nil || !ok {
		t.Fatalf("Lookup(aaa) through the mapping: ok=%v err=%v", ok, err)
	}
	if e.DF != 4500 {
		t.Fatalf("aaa df %d", e.DF)
	}
	if _, ok, _ := m.Dict.Lookup([]byte("zzz")); !ok {
		t.Fatal("inlined term missing")
	}
	if got := m.DocValues.At(0).Quality; got != 255 {
		t.Fatalf("docvalues quality %d", got)
	}
	if m.Tombstones != nil {
		t.Fatal("fresh base grew tombstones")
	}

	res := m.Residency()
	postingsOff, _, err := m.Shard.Band(hotfmt.BandPostings)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResidentPrefix != int64(postingsOff) {
		t.Fatalf("prefix %d, want postings offset %d", res.ResidentPrefix, postingsOff)
	}
	for id, want := range map[byte]int64{
		hotfmt.BandDict:      res.Dict,
		hotfmt.BandDocvalues: res.DocValues,
		hotfmt.BandMeta:      res.MetaBand,
	} {
		_, length, err := m.Shard.Band(id)
		if err != nil {
			t.Fatal(err)
		}
		if want != int64(length) {
			t.Fatalf("band %d accounted %d, band length %d", id, want, length)
		}
	}
	if res.Locked != 0 || res.WantLocked != 0 {
		t.Fatalf("MlockOff locked %d/%d bytes", res.Locked, res.WantLocked)
	}
}

func TestMountMlockTry(t *testing.T) {
	m := mountForTest(t, Options{Mlock: MlockTry})
	res := m.Residency()
	if res.WantLocked != res.ResidentPrefix {
		t.Fatalf("want-locked %d, prefix %d", res.WantLocked, res.ResidentPrefix)
	}
	// Locked is the prefix or zero depending on RLIMIT_MEMLOCK; both
	// are legal under MlockTry, partial locks are not.
	if res.Locked != 0 && res.Locked != res.ResidentPrefix {
		t.Fatalf("partial lock: %d of %d", res.Locked, res.ResidentPrefix)
	}
}

func TestHeadL1Lock(t *testing.T) {
	m := mountForTest(t, Options{Mlock: MlockTry, HeadL1Budget: 1 << 20})
	res := m.Residency()
	want := int64(hotfmt.SkipRegionSize(4500))
	if res.HeadL1Terms > 1 || (res.HeadL1 != 0 && res.HeadL1 != want) {
		t.Fatalf("head L1 locked %d bytes over %d terms, want %d over at most 1",
			res.HeadL1, res.HeadL1Terms, want)
	}
	// A budget too small for the head term locks nothing.
	m2 := mountForTest(t, Options{Mlock: MlockTry, HeadL1Budget: 16})
	if r := m2.Residency(); r.HeadL1 != 0 || r.HeadL1Terms != 0 {
		t.Fatalf("16-byte budget locked %d bytes", r.HeadL1)
	}
}

func TestMountClose(t *testing.T) {
	m := mountForTest(t, Options{Mlock: MlockOff})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if !m.Closed() {
		t.Fatal("Closed() false after Close")
	}
	if err := m.Close(); err == nil {
		t.Fatal("double close not rejected")
	}
}

func TestRegistrySwapDrains(t *testing.T) {
	reg := NewRegistry()
	m1 := mountForTest(t, Options{Mlock: MlockOff})
	if err := reg.Publish(m1); err != nil {
		t.Fatal(err)
	}
	got, release, err := reg.Acquire(7)
	if err != nil || got != m1 {
		t.Fatalf("acquire: %v", err)
	}

	m2 := mountForTest(t, Options{Mlock: MlockOff})
	if err := reg.Publish(m2); err != nil {
		t.Fatal(err)
	}
	if m1.Closed() {
		t.Fatal("old generation closed while a query still held it")
	}
	got2, release2, err := reg.Acquire(7)
	if err != nil || got2 != m2 {
		t.Fatalf("acquire after swap returned the old generation")
	}
	release()
	if !m1.Closed() {
		t.Fatal("drained old generation not closed")
	}
	release()
	release2()
	if m2.Closed() {
		t.Fatal("current generation closed by release")
	}
	if _, _, err := reg.Acquire(99); err == nil {
		t.Fatal("unknown shard acquired")
	}
}

func TestRegistryPublishIdleSwap(t *testing.T) {
	reg := NewRegistry()
	m1 := mountForTest(t, Options{Mlock: MlockOff})
	m2 := mountForTest(t, Options{Mlock: MlockOff})
	if err := reg.Publish(m1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Publish(m2); err != nil {
		t.Fatal(err)
	}
	if !m1.Closed() {
		t.Fatal("idle old generation not closed at publish")
	}
}

func TestBufPool(t *testing.T) {
	p := NewBufPool(5000)
	if p.Size() != 8192 {
		t.Fatalf("size %d, want 8192", p.Size())
	}
	b := p.Get()
	if len(b) != 8192 {
		t.Fatalf("len %d", len(b))
	}
	if uintptr(unsafe.Pointer(unsafe.SliceData(b)))%bufAlign != 0 {
		t.Fatal("buffer not 4 KiB aligned")
	}
	p.Put(b)
	p.Put(make([]byte, 8192)) // unaligned foreign buffer must be dropped
	for range 4 {
		c := p.Get()
		if uintptr(unsafe.Pointer(unsafe.SliceData(c)))%bufAlign != 0 {
			t.Fatal("pool handed out an unaligned buffer")
		}
		p.Put(c)
	}
}
