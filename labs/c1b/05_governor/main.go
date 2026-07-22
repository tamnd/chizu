// Lab governor (doc 07 sections 5 and 13): deadline compliance and
// degrade rates at budgets 6/8/10/12ms; produces the first degrade
// curve for CZ3.
//
// This is a timing lab: the deciding rows run on the gate box
// (server3) under gate-shaped concurrency, and every row names its
// host. The question it answers is narrow and mechanical: does a
// cooperative governor, checked only at read-batch boundaries, hold a
// hard budget (compliance = p100 against budget plus a serialization
// slack), and what fraction of queries it must degrade to do so, per
// class and per budget.
//
// The workload is shaped, not real: per-query block counts are drawn
// lognormal from the traversal lab's measured per-class distributions
// at 10M docs (labs/c1b/04_traversal/results, maxscore rows, qlen 3),
// each block is a real 4 KiB pread from the backing file at a random
// aligned offset plus a decode-shaped pass over the bytes, and reads
// are issued in concurrent batches of depth 32 because that is the
// planner shape doc 05 section 11 mandates. Governor checks happen
// between batches, matching "every read batch" in doc 07 section 5.
//
// The governor contract under test (doc 07 section 5):
//   - T1 (60% of budget): phase 1 must have a full heap or the
//     traversal tightens by dropping optional terms (D-terms), modeled
//     as halving the remaining phase-1 blocks.
//   - T2 (75%): phase 2 starts with whatever candidates exist; blocks
//     left on the table set D-blocks.
//   - T3 (95%): the response serializes no matter what; phase-2 work
//     left undone sets D-k.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	blockRead   = 4 << 10 // pread size per postings block (floor-dominated)
	batchDepth  = 32      // concurrent preads per read batch (planner depth)
	phase2Reads = 192     // K=64 candidates x 3 terms (doc 05 section 7)

	t1Frac = 0.60
	t2Frac = 0.75
	t3Frac = 0.95
)

// classDist is a lognormal block-count model fitted to the traversal
// lab's measured (p50, p99) per class at 10M docs, maxscore, qlen 3.
type classDist struct {
	name     string
	p50, p99 float64
	weight   float64 // share in the mix arm
}

var classes = []classDist{
	{"head", 1337, 24272, 0.30},
	{"torso", 62, 184, 0.40},
	{"tail", 5, 10, 0.25},
	{"stop", 18072, 38840, 0.05},
}

// draw returns a block count from the lognormal matching p50 and p99.
func (c classDist) draw(rng *rand.Rand) int {
	mu := math.Log(c.p50)
	sigma := (math.Log(c.p99) - mu) / 2.326
	return max(int(math.Exp(mu+sigma*rng.NormFloat64())), 1)
}

// degrade bits per doc 07 section 5.
const (
	dTerms = 1 << iota
	dK
	dBlocks
)

// gov is the per-query governor: absolute deadline, milestone marks,
// degrade bits. Checked between read batches, never preemptively.
type gov struct {
	start      time.Time
	t1, t2, t3 time.Time
	bits       uint8
}

func newGov(start time.Time, budget time.Duration) *gov {
	return &gov{
		start: start,
		t1:    start.Add(time.Duration(t1Frac * float64(budget))),
		t2:    start.Add(time.Duration(t2Frac * float64(budget))),
		t3:    start.Add(time.Duration(t3Frac * float64(budget))),
	}
}

// worker owns pooled read buffers; zero allocation on the query path.
type worker struct {
	f    *os.File
	size int64
	bufs [batchDepth][]byte
	rng  *rand.Rand
	sink uint64
}

// batch preads n blocks concurrently at random aligned offsets and
// runs a decode-shaped pass over each; returns when all land.
func (w *worker) batch(n int) {
	var wg sync.WaitGroup
	for i := range n {
		off := (w.rng.Int63n(w.size / blockRead)) * blockRead
		wg.Add(1)
		go func(buf []byte, off int64) {
			defer wg.Done()
			_, _ = w.f.ReadAt(buf, off)
		}(w.bufs[i], off)
	}
	wg.Wait()
	// Decode-shaped pass: sequential dependency over the bytes, the
	// prefix-sum structure of delta decoding.
	var acc uint64
	for i := range n {
		for _, b := range w.bufs[i] {
			acc += uint64(b) + acc>>7
		}
	}
	w.sink += acc
}

// runQuery executes one governed query and returns (latency, bits).
func (w *worker) runQuery(cl classDist, budget time.Duration) (time.Duration, uint8) {
	start := time.Now()
	g := newGov(start, budget)
	blocks := cl.draw(w.rng)
	// Heap-fill model: the top-K heap is full once ~4K postings have
	// been scored, one full batch at 128 postings per block.
	heapFullAfter := batchDepth
	done := 0
	for done < blocks {
		now := time.Now()
		if now.After(g.t2) {
			g.bits |= dBlocks
			break
		}
		if now.After(g.t1) && done < heapFullAfter {
			// T1 without a full heap: drop optional terms.
			g.bits |= dTerms
			blocks -= (blocks - done) / 2
		}
		n := min(blocks-done, batchDepth)
		w.batch(n)
		done += n
	}
	// Phase 2: 192 coalesced position reads in depth-32 batches.
	left := phase2Reads
	for left > 0 {
		if time.Now().After(g.t3) {
			g.bits |= dK
			break
		}
		n := min(left, batchDepth)
		w.batch(n)
		left -= n
	}
	// Serialization is a bounded memory pass; its cost rides in the
	// final batch above at this fidelity.
	return time.Since(start), g.bits
}

type result struct {
	lat  time.Duration
	bits uint8
}

func main() {
	label := flag.String("label", "local", "host label; timing rows are host-specific")
	path := flag.String("file", "governor.dat", "backing file (reuse lab 01's)")
	budgetMs := flag.Int("budget", 10, "query budget in ms")
	workers := flag.Int("workers", 8, "concurrent query workers (gate-shaped load)")
	queries := flag.Int("queries", 2000, "queries per class row")
	seed := flag.Int64("seed", 2107, "rng seed")
	flag.Parse()

	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open backing file:", err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stat:", err)
		os.Exit(1)
	}
	budget := time.Duration(*budgetMs) * time.Millisecond

	fmt.Println("label\tbudget_ms\tworkers\tclass\tqueries\tp50us\tp99us\tp999us\tp100us\tdeg_pct\tdterms_pct\tdk_pct\tdblocks_pct")
	arms := append([]classDist{}, classes...)
	arms = append(arms, classDist{name: "mix"})
	for _, cl := range arms {
		results := make([]result, 0, *queries)
		var mu sync.Mutex
		var wg sync.WaitGroup
		per := *queries / *workers
		for wi := range *workers {
			wg.Add(1)
			go func(wi int) {
				defer wg.Done()
				w := &worker{f: f, size: st.Size(), rng: rand.New(rand.NewSource(*seed + int64(wi)*7919))}
				for i := range w.bufs {
					w.bufs[i] = make([]byte, blockRead)
				}
				local := make([]result, 0, per)
				for range per {
					c := cl
					if cl.name == "mix" {
						c = pickMix(w.rng)
					}
					lat, bits := w.runQuery(c, budget)
					local = append(local, result{lat, bits})
				}
				mu.Lock()
				results = append(results, local...)
				mu.Unlock()
			}(wi)
		}
		wg.Wait()
		report(*label, *budgetMs, *workers, cl.name, results)
	}
}

// pickMix draws a class by the mix weights.
func pickMix(rng *rand.Rand) classDist {
	u := rng.Float64()
	acc := 0.0
	for _, c := range classes {
		acc += c.weight
		if u < acc {
			return c
		}
	}
	return classes[len(classes)-1]
}

func report(label string, budgetMs, workers int, class string, rs []result) {
	lats := make([]float64, len(rs))
	var deg, dt, dk, db int
	for i, r := range rs {
		lats[i] = float64(r.lat.Microseconds())
		if r.bits != 0 {
			deg++
		}
		if r.bits&dTerms != 0 {
			dt++
		}
		if r.bits&dK != 0 {
			dk++
		}
		if r.bits&dBlocks != 0 {
			db++
		}
	}
	sort.Float64s(lats)
	n := float64(len(rs))
	fmt.Printf("%s\t%d\t%d\t%s\t%d\t%.0f\t%.0f\t%.0f\t%.0f\t%.2f\t%.2f\t%.2f\t%.2f\n",
		label, budgetMs, workers, class, len(rs),
		pct(lats, 50), pct(lats, 99), pct(lats, 99.9), pct(lats, 100),
		100*float64(deg)/n, 100*float64(dt)/n, 100*float64(dk)/n, 100*float64(db)/n)
}

// pct returns the q-th percentile of sorted samples, nearest-rank.
func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q/100*float64(len(sorted)))) - 1
	return sorted[max(min(i, len(sorted)-1), 0)]
}
