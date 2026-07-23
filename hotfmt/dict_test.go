package hotfmt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"
)

// fixtureTerms builds a deterministic sorted vocabulary big enough for
// several blocks plus a partial last block.
func fixtureTerms(n int) [][]byte {
	terms := make([][]byte, 0, n)
	for i := range n {
		terms = append(terms, fmt.Appendf(nil, "term%04d", i))
	}
	return terms
}

// fixtureEntry alternates inline and offset entries deterministically.
func fixtureEntry(i int) DictEntry {
	if i%3 == 0 {
		df := uint32(i%4 + 1)
		e := DictEntry{DF: df}
		for j := range df {
			e.Inline = append(e.Inline, InlinePosting{
				Docid: uint32(i*100) + j*7,
				TF:    uint8(j + 1),
				Mask:  uint8(j%4 + 1),
			})
		}
		return e
	}
	return DictEntry{
		DF:          uint32(i + 5),
		CF:          uint64(i+5) * 3,
		PostingsOff: uint64(i) * 1000,
		PostingsLen: uint32(i * 17),
		SkipOff:     uint64(i) * 40,
	}
}

func buildDict(t *testing.T, terms [][]byte) []byte {
	t.Helper()
	band, err := buildDictBytes(terms)
	if err != nil {
		t.Fatal(err)
	}
	return band
}

func buildDictBytes(terms [][]byte) ([]byte, error) {
	var w DictWriter
	for i, term := range terms {
		e := fixtureEntry(i)
		if err := w.Add(term, &e); err != nil {
			return nil, fmt.Errorf("Add(%q): %w", term, err)
		}
	}
	return w.Seal()
}

func TestDictRoundTrip(t *testing.T) {
	terms := fixtureTerms(200) // 3 full blocks + 8-term tail block
	band := buildDict(t, terms)
	d, err := OpenDict(band)
	if err != nil {
		t.Fatalf("OpenDict: %v", err)
	}
	if d.NTerms() != 200 {
		t.Fatalf("NTerms = %d", d.NTerms())
	}
	i := 0
	if err := d.Walk(func(term []byte, e DictEntry) error {
		if !bytes.Equal(term, terms[i]) {
			return fmt.Errorf("term %d = %q, want %q", i, term, terms[i])
		}
		if want := fixtureEntry(i); !reflect.DeepEqual(e, want) {
			return fmt.Errorf("entry %d = %+v, want %+v", i, e, want)
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if i != 200 {
		t.Fatalf("Walk visited %d terms", i)
	}
	for j, term := range terms {
		e, ok, err := d.Lookup(term)
		if err != nil || !ok {
			t.Fatalf("Lookup(%q): ok=%v err=%v", term, ok, err)
		}
		if want := fixtureEntry(j); !reflect.DeepEqual(e, want) {
			t.Fatalf("Lookup(%q) = %+v, want %+v", term, e, want)
		}
	}
	for _, miss := range []string{"aaa", "term0000x", "term00135", "zzz"} {
		if _, ok, err := d.Lookup([]byte(miss)); ok || err != nil {
			t.Fatalf("Lookup(%q): ok=%v err=%v, want absent", miss, ok, err)
		}
	}
}

func TestDictWriterRejects(t *testing.T) {
	good := DictEntry{DF: 10, CF: 20, PostingsOff: 0, PostingsLen: 5, SkipOff: 0}
	cases := []struct {
		name string
		term []byte
		e    DictEntry
	}{
		{"empty term", nil, good},
		{"long term", bytes.Repeat([]byte("a"), maxTermLen+1), good},
		{"df zero", []byte("x"), DictEntry{}},
		{"cf below df", []byte("x"), DictEntry{DF: 10, CF: 9}},
		{"inline count skew", []byte("x"), DictEntry{DF: 2, Inline: []InlinePosting{{Docid: 1, TF: 1, Mask: 1}}}},
		{"inline with offsets", []byte("x"), DictEntry{DF: 1, PostingsOff: 8, Inline: []InlinePosting{{Docid: 1, TF: 1, Mask: 1}}}},
		{"inline disorder", []byte("x"), DictEntry{DF: 2, Inline: []InlinePosting{{Docid: 5, TF: 1, Mask: 1}, {Docid: 5, TF: 1, Mask: 1}}}},
		{"inline tf zero", []byte("x"), DictEntry{DF: 1, Inline: []InlinePosting{{Docid: 1, Mask: 1}}}},
		{"inline bad mask", []byte("x"), DictEntry{DF: 1, Inline: []InlinePosting{{Docid: 1, TF: 1, Mask: 0x10}}}},
		{"big entry inlines", []byte("x"), DictEntry{DF: 9, CF: 9, Inline: []InlinePosting{{Docid: 1, TF: 1, Mask: 1}}}},
	}
	for _, tc := range cases {
		var w DictWriter
		if err := w.Add(tc.term, &tc.e); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}

	var w DictWriter
	e := good
	if err := w.Add([]byte("mm"), &e); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Add([]byte("mm"), &e); err == nil {
		t.Error("duplicate term accepted")
	}
	if err := w.Add([]byte("aa"), &e); err == nil {
		t.Error("out-of-order term accepted")
	}
	var empty DictWriter
	if _, err := empty.Seal(); err == nil {
		t.Error("empty dictionary sealed")
	}
}

func TestOpenDictRejectsCorruption(t *testing.T) {
	band := buildDict(t, fixtureTerms(200))
	if _, err := OpenDict(band[:8]); err == nil {
		t.Error("short band accepted")
	}
	if _, err := OpenDict(band[:len(band)-1]); err == nil {
		t.Error("truncated trailer accepted")
	}
	grown := append(bytes.Clone(band), band[len(band)-dictTrailerSize:]...)
	if _, err := OpenDict(grown); err == nil {
		t.Error("band with stale interior trailer accepted")
	}
	bad := bytes.Clone(band)
	binary.LittleEndian.PutUint32(bad[len(bad)-12:], 99) // nblocks
	if _, err := OpenDict(bad); err == nil {
		t.Error("wrong block count accepted")
	}
}

// TestDictRejectsNonCanonicalFrontCoding hand-builds a two-term block
// where the second term is stored with a shorter lcp than the true
// common prefix, a valid ordering but not the canonical encoding.
func TestDictRejectsNonCanonicalFrontCoding(t *testing.T) {
	e := DictEntry{DF: 10, CF: 20}
	var band []byte
	band = append(band, 2)
	band = append(band, "ab"...)
	band = append(band, 0, 2) // lcp 0 for "ac": true lcp is 1
	band = append(band, "ac"...)
	band = appendDictEntry(band, &e)
	band = appendDictEntry(band, &e)
	indexOff := uint64(len(band))
	band = appendU48(band, 0)
	band = append(band, 2)
	band = append(band, "ab"...)
	band = binary.LittleEndian.AppendUint64(band, indexOff)
	band = binary.LittleEndian.AppendUint32(band, 1)
	band = binary.LittleEndian.AppendUint64(band, 2)

	d, err := OpenDict(band)
	if err != nil {
		t.Fatalf("OpenDict: %v", err)
	}
	if err := d.Walk(func([]byte, DictEntry) error { return nil }); err == nil {
		t.Fatal("non-canonical front-coding accepted")
	}
}

func FuzzOpenDict(f *testing.F) {
	if band, err := buildDictBytes(fixtureTerms(200)); err == nil {
		f.Add(band)
	}
	if band, err := buildDictBytes([][]byte{[]byte("only")}); err == nil {
		f.Add(band)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := OpenDict(data)
		if err != nil {
			return
		}
		var w DictWriter
		if err := d.Walk(func(term []byte, e DictEntry) error {
			return w.Add(term, &e)
		}); err != nil {
			return
		}
		out, err := w.Seal()
		if err != nil {
			t.Fatalf("re-seal of accepted dictionary: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("accepted dictionary is not canonical: %d bytes in, %d out", len(data), len(out))
		}
	})
}

func TestLookupInto(t *testing.T) {
	terms := fixtureTerms(200)
	d, err := OpenDict(buildDict(t, terms))
	if err != nil {
		t.Fatal(err)
	}
	inline := make([]InlinePosting, 0, 4)
	for i, term := range terms {
		want, ok, err := d.Lookup(term)
		if err != nil || !ok {
			t.Fatalf("Lookup(%q): ok=%v err=%v", term, ok, err)
		}
		var got DictEntry
		ok, err = d.LookupInto(term, &got, inline)
		if err != nil || !ok {
			t.Fatalf("LookupInto(%q): ok=%v err=%v", term, ok, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("term %d %q: entries diverge\nlookup     %+v\nlookupinto %+v", i, term, want, got)
		}
	}
	for _, miss := range [][]byte{[]byte("aaaa"), []byte("term0100a"), []byte("zzzz")} {
		var e DictEntry
		if ok, err := d.LookupInto(miss, &e, inline); ok || err != nil {
			t.Fatalf("LookupInto(%q): ok=%v err=%v", miss, ok, err)
		}
	}
	var e DictEntry
	if _, err := d.LookupInto(terms[0], &e, nil); err == nil {
		t.Fatal("inline entry accepted a nil inline buffer")
	}
}
