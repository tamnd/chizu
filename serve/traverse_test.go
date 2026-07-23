package serve

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/tamnd/chizu/hotfmt"
)

// travDocs is the traversal fixture's doc space: small enough to brute
// force, large enough for multi-block terms and real skip work.
const travDocs = 20_000

// travTerm is one generated term: the raw postings stay in test memory
// as the brute-force reference.
type travTerm struct {
	name string
	ps   []hotfmt.Posting
}

// genTravTerms draws a Zipf-ish vocabulary: a few head terms spanning
// many blocks, a torso, and inline-class tail terms. Impact equals TF,
// the emit pass's rule this generation.
func genTravTerms(seed int64) []travTerm {
	rng := rand.New(rand.NewSource(seed))
	dfs := []int{6000, 3500, 1500, 700, 300, 150, 129, 128, 90, 40, 12, 5, 4, 2, 1}
	terms := make([]travTerm, len(dfs))
	for ti, df := range dfs {
		name := fmt.Sprintf("t%02d", ti)
		ps := make([]hotfmt.Posting, 0, df)
		// df+1 keeps even a one-posting term inside the doc space at
		// the draw's 1.5x worst case.
		stride := float64(travDocs) / float64(df+1)
		pos := 0.0
		for range df {
			pos += stride * (0.5 + rng.Float64())
			d := uint32(pos)
			if d >= travDocs {
				break
			}
			// tf-shaped impacts: most postings low, a few near the
			// ceiling, which is what makes block-max pruning real.
			f := rng.Float64()
			tf := uint8(1 + f*f*f*30)
			ps = append(ps, hotfmt.Posting{Docid: d, TF: tf, Mask: 1, Impact: tf})
		}
		terms[ti] = travTerm{name: name, ps: ps}
	}
	return terms
}

// buildTravShard writes a .hot holding the generated terms, inline for
// df <= 4 like the real dictionary rule.
func buildTravShard(t *testing.T, terms []travTerm) string {
	t.Helper()
	var postingsBand, skipsBand, positionsBand []byte
	var dw hotfmt.DictWriter
	names := make([]string, len(terms))
	byName := map[string]travTerm{}
	for i, tt := range terms {
		names[i] = tt.name
		byName[tt.name] = tt
	}
	sort.Strings(names)
	for _, name := range names {
		tt := byName[name]
		e := &hotfmt.DictEntry{DF: uint32(len(tt.ps))}
		if len(tt.ps) <= 4 {
			for _, p := range tt.ps {
				e.Inline = append(e.Inline, hotfmt.InlinePosting{Docid: p.Docid, TF: p.TF, Mask: p.Mask})
			}
		} else {
			enc, l1, err := hotfmt.EncodePostings(tt.ps)
			if err != nil {
				t.Fatalf("%s postings: %v", name, err)
			}
			for _, p := range tt.ps {
				e.CF += uint64(p.TF)
			}
			posOffs := make([]uint64, len(l1))
			for bi := range l1 {
				posOffs[bi] = uint64(len(positionsBand))
				positionsBand = append(positionsBand, 0)
			}
			e.PostingsOff = uint64(len(postingsBand))
			e.PostingsLen = uint32(len(enc))
			e.SkipOff = uint64(len(skipsBand))
			postingsBand = append(postingsBand, enc...)
			skipsBand, err = hotfmt.EncodeSkips(skipsBand, l1, posOffs)
			if err != nil {
				t.Fatalf("%s skips: %v", name, err)
			}
		}
		if err := dw.Add([]byte(name), e); err != nil {
			t.Fatalf("dict %s: %v", name, err)
		}
	}
	dictBand, err := dw.Seal()
	if err != nil {
		t.Fatal(err)
	}

	dvs := make([]hotfmt.DocValue, travDocs)
	for i := range dvs {
		dvs[i] = hotfmt.DocValue{Quality: 200, Lang: 1, DoclenBody: 7}
	}
	dvBand, err := hotfmt.EncodeDocValues(dvs)
	if err != nil {
		t.Fatal(err)
	}
	dbw := hotfmt.NewDocBandWriter(t.TempDir())
	if err := dbw.Add(hotfmt.DocRecord{
		URL: []byte("https://example.test/0"), Title: []byte("Doc 0"),
		Snippet: []byte("Snippet source for document zero, long enough to be real text."),
	}); err != nil {
		t.Fatal(err)
	}
	var docBand sortableBuffer
	if err := dbw.Seal(&docBand); err != nil {
		t.Fatal(err)
	}

	h := &hotfmt.FileHeader{
		Shard: 7, Generation: 12, Kind: hotfmt.KindBase,
		DocCount: travDocs, TermCount: uint64(len(terms)), TokenizerVer: 2,
		QuantScale: 0.125, Writer: 3,
	}
	meta := &hotfmt.Meta{
		LawVer: 1, TokenizerVer: 2, QuantScale: 0.125, QuantPolicy: 1,
		DocCount: travDocs,
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
		hotfmt.BandDocband:   docBand.b,
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

type sortableBuffer struct{ b []byte }

func (s *sortableBuffer) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// travMount mounts the generated shard once per test.
func travMount(t *testing.T, terms []travTerm) *Mount {
	t.Helper()
	m, err := MountShard(buildTravShard(t, terms), Options{Mlock: MlockOff})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// openWorker resets the worker and opens the query's cursors.
func openWorker(t *testing.T, w *Worker, m *Mount, terms []travTerm, names []string) {
	t.Helper()
	byName := map[string]travTerm{}
	for _, tt := range terms {
		byName[tt.name] = tt
	}
	w.Reset()
	for _, name := range names {
		c, ok, err := w.OpenTerm(m, []byte(name))
		if err != nil || !ok {
			t.Fatalf("OpenTerm(%s): ok=%v err=%v", name, ok, err)
		}
		if want := uint32(len(byName[name].ps)); c.DF() != want {
			t.Fatalf("%s df %d, want %d", name, c.DF(), want)
		}
	}
}

// brute computes the exact top-k of the term union, honoring deleted.
func brute(terms []travTerm, names []string, k int, deleted func(uint32) bool) []Hit {
	byName := map[string]travTerm{}
	for _, tt := range terms {
		byName[tt.name] = tt
	}
	acc := map[uint32]int32{}
	for _, name := range names {
		for _, p := range byName[name].ps {
			acc[p.Docid] += int32(p.TF)
		}
	}
	var hits []Hit
	for d, s := range acc {
		if deleted != nil && deleted(d) {
			continue
		}
		hits = append(hits, Hit{d, s})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Docid < hits[j].Docid
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// sameScores compares score sequences: rank safety is about scores,
// ties may legally resolve to different docids.
func sameScores(gold, got []Hit) bool {
	if len(gold) != len(got) {
		return false
	}
	for i := range gold {
		if gold[i].Score != got[i].Score {
			return false
		}
	}
	return true
}

func TestCursorWalk(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	w := NewWorker()
	for _, tt := range terms {
		openWorker(t, w, m, terms, []string{tt.name})
		cur := &w.cursors()[0]
		for i, p := range tt.ps {
			if got := cur.Docid(); got != p.Docid {
				t.Fatalf("%s posting %d: docid %d, want %d", tt.name, i, got, p.Docid)
			}
			if got := cur.Impact(); got != p.TF {
				t.Fatalf("%s posting %d: impact %d, want %d", tt.name, i, got, p.TF)
			}
			cur.Next()
		}
		if got := cur.Docid(); got != docidExhausted {
			t.Fatalf("%s: docid %d past the end", tt.name, got)
		}
		if cur.Err() != nil {
			t.Fatalf("%s: %v", tt.name, cur.Err())
		}
	}
}

func TestCursorAdvance(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	rng := rand.New(rand.NewSource(7))
	w := NewWorker()
	for _, tt := range terms {
		openWorker(t, w, m, terms, []string{tt.name})
		cur := &w.cursors()[0]
		target := uint32(0)
		for range 40 {
			target += uint32(rng.Intn(travDocs / 20))
			// Reference: first posting at docid >= target.
			want := docidExhausted
			var wantImp uint8
			for _, p := range tt.ps {
				if p.Docid >= target {
					want, wantImp = p.Docid, p.TF
					break
				}
			}
			if bb := cur.BlockBound(target); want != docidExhausted && bb < wantImp {
				t.Fatalf("%s: block bound %d below impact %d at %d", tt.name, bb, wantImp, target)
			}
			cur.Advance(target)
			if got := cur.Docid(); got != want {
				t.Fatalf("%s: Advance(%d) landed on %d, want %d", tt.name, target, got, want)
			}
			if want == docidExhausted {
				break
			}
		}
		if cur.Err() != nil {
			t.Fatalf("%s: %v", tt.name, cur.Err())
		}
	}
}

func TestTraverseRankSafety(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	rng := rand.New(rand.NewSource(11))
	for q := range 60 {
		qlen := 1 + q%5
		names := map[string]bool{}
		for len(names) < qlen {
			names[terms[rng.Intn(len(terms))].name] = true
		}
		var qn []string
		for n := range names {
			qn = append(qn, n)
		}
		sort.Strings(qn)
		for _, k := range []int{10, 100} {
			gold := brute(terms, qn, k, nil)
			for algo, run := range map[string]func(*Worker, int, func(uint32) bool) ([]Hit, error){
				"maxscore": (*Worker).TraverseMaxScore,
				"bmw":      (*Worker).TraverseBMW,
				"hybrid":   (*Worker).Traverse,
			} {
				w := NewWorker()
				openWorker(t, w, m, terms, qn)
				got, err := run(w, k, nil)
				if err != nil {
					t.Fatalf("%s %v k=%d: %v", algo, qn, k, err)
				}
				if !sameScores(gold, got) {
					t.Fatalf("%s %v k=%d: scores diverge from exhaustive\ngold %v\ngot  %v",
						algo, qn, k, gold, got)
				}
			}
		}
	}
}

func TestTraverseDeleted(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	deleted := func(d uint32) bool { return d%7 == 0 }
	qn := []string{"t00", "t04", "t09"}
	gold := brute(terms, qn, 50, deleted)
	w := NewWorker()
	openWorker(t, w, m, terms, qn)
	got, err := w.Traverse(50, deleted)
	if err != nil {
		t.Fatal(err)
	}
	if !sameScores(gold, got) {
		t.Fatalf("deleted-aware scores diverge\ngold %v\ngot  %v", gold, got)
	}
	for _, h := range got {
		if deleted(h.Docid) {
			t.Fatalf("deleted doc %d surfaced", h.Docid)
		}
	}
}

// TestTraversePrunes pins the point of the block-max machinery: a
// selective query decodes fewer blocks than exist, and bound checks
// outnumber decodes on the pruned side.
func TestTraversePrunes(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	// Rare term drives the threshold, head term gets pruned: the
	// two-term shape BMW owns per the lab 04 crossover.
	qn := []string{"t00", "t10"}
	total := 0
	for _, tt := range terms {
		if tt.name == "t00" || tt.name == "t10" {
			total += (len(tt.ps) + hotfmt.PostingsBlockLen - 1) / hotfmt.PostingsBlockLen
		}
	}
	w := NewWorker()
	openWorker(t, w, m, terms, qn)
	hits, err := w.Traverse(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 10 {
		t.Fatalf("got %d hits", len(hits))
	}
	if w.Stats.Blocks >= int64(total) {
		t.Fatalf("no pruning: %d blocks decoded of %d", w.Stats.Blocks, total)
	}
	if w.Stats.Checks == 0 {
		t.Fatal("no bound checks recorded")
	}
	t.Logf("blocks %d of %d, scored %d, checks %d", w.Stats.Blocks, total, w.Stats.Scored, w.Stats.Checks)
}

func TestOpenTermMissing(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	w := NewWorker()
	c, ok, err := w.OpenTerm(m, []byte("nosuchterm"))
	if err != nil {
		t.Fatal(err)
	}
	if ok || c != nil {
		t.Fatal("absent term produced a cursor")
	}
	if w.open != 0 {
		t.Fatal("absent term consumed a slot")
	}
}

// TestWorkerZeroAlloc is the doc 07 section 9 discipline as a number:
// after one warmup query grows the worker to its high-water marks, the
// steady query path allocates nothing.
func TestWorkerZeroAlloc(t *testing.T) {
	terms := genTravTerms(2107)
	m := travMount(t, terms)
	w := NewWorker()
	queries := [][][]byte{
		{[]byte("t01")},                // bmw single
		{[]byte("t00"), []byte("t10")}, // bmw pair
		{[]byte("t02"), []byte("t05"), []byte("t08"), []byte("t12")}, // maxscore
		{[]byte("t03"), []byte("t11"), []byte("t13"), []byte("t14")}, // inline tail terms
	}
	run := func() {
		for _, q := range queries {
			w.Reset()
			for _, term := range q {
				if _, ok, err := w.OpenTerm(m, term); err != nil || !ok {
					t.Fatalf("OpenTerm(%s): ok=%v err=%v", term, ok, err)
				}
			}
			if _, err := w.Traverse(10, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	run() // warmup: arena and heap reach their high-water marks
	if allocs := testing.AllocsPerRun(50, run); allocs != 0 {
		t.Fatalf("steady query path allocates %.1f times per run, want 0", allocs)
	}
}
