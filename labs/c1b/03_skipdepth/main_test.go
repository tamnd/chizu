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

func TestPageAlign(t *testing.T) {
	if pageAlign(0) != 0 {
		t.Fatal("pageAlign(0) != 0")
	}
	if pageAlign(1) != page || pageAlign(page) != page || pageAlign(page+1) != 2*page {
		t.Fatal("pageAlign not rounding to the pread floor")
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
	const docs = 500_000_000
	q := []term{mkTerm(200_000, docs), mkTerm(500, docs), mkTerm(2, docs)}
	for _, survive := range []float64{1, 0.1, 0.01} {
		arms := simQuery(q, survive, rand.New(rand.NewSource(7)))
		full, span, hyb := arms[0], arms[1], arms[2]
		if full.blocks != span.blocks || full.blocks != hyb.blocks {
			t.Fatalf("survive %v: blocks differ across arms: %d %d %d", survive, full.blocks, span.blocks, hyb.blocks)
		}
		if hyb.l1 > full.l1 || hyb.l1 > span.l1 {
			t.Fatalf("survive %v: hybrid l1 %d exceeds an arm it chooses from (%d, %d)", survive, hyb.l1, full.l1, span.l1)
		}
		if full.l1 <= 0 || full.preads <= 0 || full.blocks <= 0 {
			t.Fatalf("survive %v: degenerate l1only cost: %+v", survive, full)
		}
		if survive == 1 && span.l1 < full.l1/2 {
			t.Fatalf("no pruning should keep spans near the full array for a dense driver: span %d vs full %d", span.l1, full.l1)
		}
	}

	// Strong pruning at 500M scale is where spans win: rare driver
	// against a head term with a multi-MB L1 array.
	rare := simQuery(q, 0.01, rand.New(rand.NewSource(7)))
	if rare[2].l1 >= rare[0].l1 {
		t.Fatalf("hybrid %d not below l1only %d at survive 0.01", rare[2].l1, rare[0].l1)
	}
}

func TestSimQueryDeterministic(t *testing.T) {
	q := []term{mkTerm(50_000, 10_000_000), mkTerm(100, 10_000_000)}
	a := simQuery(q, 0.1, rand.New(rand.NewSource(7)))
	b := simQuery(q, 0.1, rand.New(rand.NewSource(7)))
	if a != b {
		t.Fatalf("same-seed runs differ: %+v vs %+v", a, b)
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
