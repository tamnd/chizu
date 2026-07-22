package main

import (
	"math/rand"
	"testing"
)

func TestDocFreq(t *testing.T) {
	const docs = 10_000_000
	if df := docFreq(1, docs); df != int64(0.45*docs) {
		t.Fatalf("rank 1 df = %d, want %d", df, int64(0.45*docs))
	}
	prev := int64(1 << 62)
	for _, r := range []int64{1, 10, 100, 10_000, vocab} {
		df := docFreq(r, docs)
		if df < 1 || df > prev {
			t.Fatalf("rank %d df = %d not in (0, %d]", r, df, prev)
		}
		prev = df
	}
}

func TestGenQueriesDeterministic(t *testing.T) {
	a := genQueries("rand", 3, 50, 10_000_000, 7)
	b := genQueries("rand", 3, 50, 10_000_000, 7)
	if len(a) != 50 {
		t.Fatalf("got %d queries, want 50", len(a))
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("query %d term %d differs across same-seed runs", i, j)
			}
			if j > 0 && a[i][j].df < a[i][j-1].df {
				t.Fatalf("query %d not sorted by df", i)
			}
		}
	}
	for _, q := range genQueries("stop", 3, 50, 10_000_000, 7) {
		for _, tm := range q {
			if tm.rank > 50 {
				t.Fatalf("stop class drew rank %d, want <= 50", tm.rank)
			}
		}
	}
}

func TestSimQueryArms(t *testing.T) {
	const docs = 10_000_000
	q := []term{mkTerm(50_000, docs), mkTerm(100, docs), mkTerm(5, docs)}
	full := simQuery(q, docs, "l1only", 0, rand.New(rand.NewSource(7)))
	span := simQuery(q, docs, "l1l2", 1000, rand.New(rand.NewSource(7)))
	if full.blocks != span.blocks {
		t.Fatalf("blocks differ across arms: %d vs %d (same seed, same traversal)", full.blocks, span.blocks)
	}
	if span.l1 >= full.l1 {
		t.Fatalf("l1l2 skip bytes %d not below l1only %d for a rare driver", span.l1, full.l1)
	}
	if full.l1 <= 0 || full.preads <= 0 || full.blocks <= 0 {
		t.Fatalf("degenerate cost: %+v", full)
	}
	knob0 := simQuery(q, docs, "l1l2", 0, rand.New(rand.NewSource(7)))
	if knob0.preads < span.preads || knob0.l1 < span.l1 {
		t.Fatalf("knob 0 (no resident L2) should cost at least knob 1000: %+v vs %+v", knob0, span)
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
