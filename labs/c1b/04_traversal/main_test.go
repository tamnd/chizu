package main

import (
	"math/rand"
	"testing"
)

const testDocs = 200_000

func testQuery(ranks ...int64) []term {
	q := make([]term, 0, len(ranks))
	for _, r := range ranks {
		q = append(q, mkTerm(r, testDocs, 2107))
	}
	return q
}

func TestRankSafety(t *testing.T) {
	// Every algorithm must reproduce the exhaustive top-K score
	// sequence exactly, across bands and query shapes.
	cases := [][]int64{
		{5, 12},
		{1, 3, 7},
		{100, 2000, 50_000},
		{30_000, 80_000, 200_000, 500_000},
		{1, 2, 3, 4, 5},
		{999_999},
	}
	for _, ranks := range cases {
		q := testQuery(ranks...)
		gold, _ := exhaustive(q, testDocs)
		ms, _ := maxscore(q)
		bw, _ := bmw(q)
		if !sameTop(gold, ms) {
			t.Fatalf("maxscore unsafe on ranks %v: gold %d hits, got %d", ranks, len(gold), len(ms))
		}
		if !sameTop(gold, bw) {
			t.Fatalf("bmw unsafe on ranks %v: gold %d hits, got %d", ranks, len(gold), len(bw))
		}
	}
}

func TestRankSafetyRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for range 30 {
		q := genQuery(rng, 1, vocab, 2+rng.Intn(3), testDocs)
		gold, _ := exhaustive(q, testDocs)
		ms, _ := maxscore(q)
		bw, _ := bmw(q)
		if !sameTop(gold, ms) || !sameTop(gold, bw) {
			t.Fatalf("unsafe on random query %v", ranks(q))
		}
	}
}

func ranks(q []term) []int64 {
	out := make([]int64, len(q))
	for i, tm := range q {
		out[i] = tm.rank
	}
	return out
}

func TestPruningDoesWork(t *testing.T) {
	// A rare term with stopwords at a small K: both pruned algorithms
	// must score fewer postings than exhaustive, and never decode more
	// blocks. K=10 here because at 200k docs a K=1000 heap threshold
	// sits below the stopword bound sums and pruning barely engages;
	// the real crossover numbers come from the sweep at full scale.
	old := topK
	topK = 10
	defer func() { topK = old }()
	q := testQuery(80_000, 3, 7)
	_, exc := exhaustive(q, testDocs)
	_, msc := maxscore(q)
	_, bwc := bmw(q)
	if msc.scored >= exc.scored {
		t.Fatalf("maxscore scored %d of %d exhaustive postings; pruning not working", msc.scored, exc.scored)
	}
	if bwc.scored*2 >= exc.scored {
		t.Fatalf("bmw scored %d of %d exhaustive postings; pruning not working", bwc.scored, exc.scored)
	}
	if msc.blocks > exc.blocks || bwc.blocks > exc.blocks {
		t.Fatalf("pruned run decoded more blocks than exhaustive: ms %d, bmw %d, ex %d", msc.blocks, bwc.blocks, exc.blocks)
	}
}

func TestTermConstruction(t *testing.T) {
	tm := mkTerm(50, testDocs, 2107)
	if len(tm.docids) == 0 || len(tm.docids) != len(tm.impacts) {
		t.Fatalf("term shape: %d docids, %d impacts", len(tm.docids), len(tm.impacts))
	}
	for i := 1; i < len(tm.docids); i++ {
		if tm.docids[i] <= tm.docids[i-1] {
			t.Fatalf("docids not strictly increasing at %d", i)
		}
	}
	for i, imp := range tm.impacts {
		b := i / blockLen
		if imp > tm.blkMax[b] {
			t.Fatalf("posting %d impact %d exceeds its block max %d", i, imp, tm.blkMax[b])
		}
		if imp > tm.bound {
			t.Fatalf("posting %d impact %d exceeds the term bound %d", i, imp, tm.bound)
		}
	}
	again := mkTerm(50, testDocs, 2107)
	if len(again.docids) != len(tm.docids) || again.bound != tm.bound {
		t.Fatal("mkTerm not deterministic")
	}
}

func TestPct(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := pct(sorted, 50); got != 5 {
		t.Fatalf("p50 = %v, want 5", got)
	}
	if got := pct(sorted, 99.9); got != 10 {
		t.Fatalf("p99.9 = %v, want 10", got)
	}
	if got := pct(nil, 50); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
}
