package main

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/tamnd/chizu/hotfmt"
)

// The lab writer at block 64 must produce the exact bytes hotfmt's
// DictWriter produces, or the sweep measures a drifted codec.
func TestLabDictMatchesHotfmt(t *testing.T) {
	rng := rand.New(rand.NewSource(2107))
	terms := fixtureVocab(rng, 20000)
	entries := makeEntries(rng, len(terms))

	lw := &labDictWriter{blockSize: 64}
	hw := &hotfmt.DictWriter{}
	for i, term := range terms {
		if err := lw.add(term, &entries[i]); err != nil {
			t.Fatalf("lab add %q: %v", term, err)
		}
		if err := hw.Add(term, hotEntry(&entries[i])); err != nil {
			t.Fatalf("hotfmt add %q: %v", term, err)
		}
	}
	lb, err := lw.seal()
	if err != nil {
		t.Fatalf("lab seal: %v", err)
	}
	hb, err := hw.Seal()
	if err != nil {
		t.Fatalf("hotfmt seal: %v", err)
	}
	if !bytes.Equal(lb, hb) {
		n := min(len(lb), len(hb))
		i := 0
		for i < n && lb[i] == hb[i] {
			i++
		}
		t.Fatalf("bands differ: lab %d bytes, hotfmt %d bytes, first diff at %d", len(lb), len(hb), i)
	}
}

// Lab lookup at 64 and hotfmt.Dict.Lookup must agree on hits and
// misses over the same band.
func TestLookupAgreesWithHotfmt(t *testing.T) {
	rng := rand.New(rand.NewSource(2107))
	terms := fixtureVocab(rng, 5000)
	entries := makeEntries(rng, len(terms))
	_, band := buildLabT(t, terms, entries, 64)

	ld, err := openLabDict(band, 64)
	if err != nil {
		t.Fatalf("openLabDict: %v", err)
	}
	hd, err := hotfmt.OpenDict(band)
	if err != nil {
		t.Fatalf("hotfmt.OpenDict: %v", err)
	}

	for i, term := range terms {
		le, lok, lerr := ld.lookup(term)
		he, hok, herr := hd.Lookup(term)
		if lerr != nil || herr != nil {
			t.Fatalf("lookup %q: lab err %v, hotfmt err %v", term, lerr, herr)
		}
		if !lok || !hok {
			t.Fatalf("lookup %q: lab ok %v, hotfmt ok %v", term, lok, hok)
		}
		if !entryEqualHot(&le, &he) {
			t.Fatalf("term %d %q: entries disagree", i, term)
		}
		if !entryEqualLab(&le, &entries[i]) {
			t.Fatalf("term %d %q: entry does not round-trip", i, term)
		}
	}

	for _, miss := range []string{"", "zzzzzzzzzzzzzz", "\x00a", "not-in-vocab-xq"} {
		_, lok, lerr := ld.lookup([]byte(miss))
		_, hok, herr := hd.Lookup([]byte(miss))
		if lerr != nil || herr != nil {
			t.Fatalf("miss %q: lab err %v, hotfmt err %v", miss, lerr, herr)
		}
		if lok || hok {
			t.Fatalf("miss %q: lab ok %v, hotfmt ok %v", miss, lok, hok)
		}
	}
}

// Every swept block size must return the right entry for every term.
func TestLookupAllBlockSizes(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	terms := fixtureVocab(rng, 3000)
	entries := makeEntries(rng, len(terms))
	for _, bs := range []int{32, 64, 128} {
		_, band := buildLabT(t, terms, entries, bs)
		d, err := openLabDict(band, bs)
		if err != nil {
			t.Fatalf("block %d: openLabDict: %v", bs, err)
		}
		for i, term := range terms {
			e, ok, err := d.lookup(term)
			if err != nil || !ok {
				t.Fatalf("block %d term %q: ok %v err %v", bs, term, ok, err)
			}
			if !entryEqualLab(&e, &entries[i]) {
				t.Fatalf("block %d term %q: wrong entry", bs, term)
			}
		}
	}
}

func TestDafsaMembership(t *testing.T) {
	words := []string{"tap", "taps", "top", "tops", "trap", "traps"}
	d := newDafsa()
	for _, w := range words {
		d.add([]byte(w))
	}
	root := d.finish()
	for _, w := range words {
		if !d.contains([]byte(w)) {
			t.Fatalf("missing %q", w)
		}
	}
	for _, w := range []string{"ta", "tapss", "to", "trapse", "x", ""} {
		if d.contains([]byte(w)) {
			t.Fatalf("false member %q", w)
		}
	}
	states, arcs, size := sizeDafsa(root)
	// tap/top/trap share the "s"-optional tail: minimization must merge
	// the three final-with-s-arc states into one.
	if states >= 1+len(words)*3 {
		t.Fatalf("automaton not minimized: %d states", states)
	}
	if arcs == 0 || size == 0 {
		t.Fatalf("empty sizing: %d arcs %d bytes", arcs, size)
	}
}

func TestVocabDeterministicSorted(t *testing.T) {
	a := fixtureVocab(rand.New(rand.NewSource(7)), 5000)
	b := fixtureVocab(rand.New(rand.NewSource(7)), 5000)
	if len(a) != 5000 || len(b) != 5000 {
		t.Fatalf("sizes %d %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("vocab not deterministic at %d", i)
		}
		if i > 0 && bytes.Compare(a[i-1], a[i]) >= 0 {
			t.Fatalf("vocab not strictly increasing at %d", i)
		}
		if len(a[i]) == 0 || len(a[i]) > labMaxTermLen {
			t.Fatalf("term %d length %d", i, len(a[i]))
		}
	}
}

func buildLabT(t *testing.T, terms [][]byte, entries []labEntry, blockSize int) (*labDictWriter, []byte) {
	t.Helper()
	w := &labDictWriter{blockSize: blockSize}
	for i, term := range terms {
		if err := w.add(term, &entries[i]); err != nil {
			t.Fatalf("add %q: %v", term, err)
		}
	}
	band, err := w.seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return w, band
}

func entryEqualLab(a, b *labEntry) bool {
	if a.DF != b.DF || a.CF != b.CF || a.PostingsOff != b.PostingsOff ||
		a.PostingsLen != b.PostingsLen || a.SkipOff != b.SkipOff ||
		len(a.Inline) != len(b.Inline) {
		return false
	}
	for i := range a.Inline {
		if a.Inline[i] != b.Inline[i] {
			return false
		}
	}
	return true
}

func entryEqualHot(a *labEntry, b *hotfmt.DictEntry) bool {
	if a.DF != b.DF || a.CF != b.CF || a.PostingsOff != b.PostingsOff ||
		a.PostingsLen != b.PostingsLen || a.SkipOff != b.SkipOff ||
		len(a.Inline) != len(b.Inline) {
		return false
	}
	for i := range a.Inline {
		if a.Inline[i].Docid != b.Inline[i].Docid || a.Inline[i].TF != b.Inline[i].TF || a.Inline[i].Mask != b.Inline[i].Mask {
			return false
		}
	}
	return true
}
