// Lab skip-depth (doc 05 sections 6 and 14): blocks read and bytes
// pread per query at L1-only versus L1+L2 on Zipf-realistic fixtures.
// Gates the two-level skip design, the superskip residency knob
// default, and the section 6 worst-case traversal arithmetic.
//
// This is a counting simulation, not a timing run: every row is a
// deterministic function of the seed, so the numbers are
// host-independent and carry no perf claim. The model:
//
//   - Terms have Zipf docfreq df(rank) over N docs; postings spread
//     uniformly, 128 per block, so a docid maps to a block index by
//     quantile. Query terms are drawn Zipf over the vocabulary (query
//     logs are head-heavy too), plus an adversarial all-stopword class.
//   - The rarest term drives: it reads all its blocks and its whole L1
//     array (sequential, identical in every arm). Every other term is
//     probed at the driver's candidates, Poissonized per L1 entry.
//   - survive models block-max pruning strength: a probed block reads
//     its postings with probability s, and its resident L2 entry (span
//     max over 32 blocks) passes with probability 1-(1-s)^32, drawn
//     correlated so a passing block always has a passing span. survive
//     1 is the no-pruning worst case; doc 05's 1-3 MB arithmetic
//     assumes MaxScore-style bounds, which is why sweep 1 (no pruning,
//     no floors) measured 60-85 MB against it.
//   - Every skip read pays the 4 KiB pread floor, and runs of touched
//     L1 entries closer than 4 KiB coalesce into one pread.
//
// Arms:
//
//   - l1only: a probed term preads its entire L1 array, because
//     nothing resident says where to look.
//   - l1l2: resident L2 names the L1 spans; only spans whose L2 entry
//     passes are read. Terms outside the residency knob (top-1000 df
//     ranks keep L2 mlocked) pay one extra pread for their L2 region.
//   - hybrid: the planner knows df and the candidate count up front
//     and picks span-reads or the full array per term, whichever costs
//     fewer bytes. This is what the read-planner slice would bake.
//
// Output is TSV: label docs arm survive class qterms queries l1kb_p50
// l1kb_p99 preads_p50 preads_p99 blocks_p50 blocks_p99 nvme_mb_p99.
// l1kb is skip-band bytes pread; nvme_mb adds postings blocks at the
// doc 05 compression accounting (~1.3 KB per block).
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const (
	blockLen   = 128       // postings per block
	l1Bytes    = 15        // L1 entry: 10 skip + 5 positions offset
	l2Bytes    = 12        // padded L2 entry
	l2Fan      = 32        // L1 entries per L2 entry
	blockBytes = 1331      // ~1.3 KB compressed postings block (doc 05 section 6)
	page       = 4096      // pread floor and run-coalesce gap
	knob       = 1000      // L2 resident for top-knob df ranks (P3, sweep 1)
	vocab      = 1_000_000 // term ranks drawn 1..vocab
)

type term struct {
	rank int64
	df   int64
	nb   int64 // ceil(df/128), L1 entries = postings blocks
}

type cost struct {
	l1     int64 // skip-band bytes pread
	preads int64
	blocks int64 // postings blocks read
}

func (c *cost) add(o cost) {
	c.l1 += o.l1
	c.preads += o.preads
	c.blocks += o.blocks
}

func main() {
	label := flag.String("label", "local", "row label; rows are host-independent counts")
	docs := flag.Int64("docs", 10_000_000, "simulated shard doc count")
	queries := flag.Int("queries", 400, "queries per config")
	seed := flag.Int64("seed", 2107, "rng seed")
	flag.Parse()

	type class struct {
		name  string
		qlens []int
	}
	classes := []class{
		{"rand", []int{2, 3, 4}},
		{"stop", []int{3}},
	}
	arms := []string{"l1only", "l1l2", "hybrid"}

	fmt.Println("label\tdocs\tarm\tsurvive\tclass\tqterms\tqueries\tl1kb_p50\tl1kb_p99\tpreads_p50\tpreads_p99\tblocks_p50\tblocks_p99\tnvme_mb_p99")
	for _, cl := range classes {
		for _, qlen := range cl.qlens {
			qs := genQueries(cl.name, qlen, *queries, *docs, *seed)
			for _, survive := range []float64{1, 0.1, 0.01} {
				samples := map[string][][]float64{}
				rng := rand.New(rand.NewSource(*seed + 1))
				for _, q := range qs {
					perArm := simQuery(q, survive, rng)
					for i, a := range arms {
						c := perArm[i]
						samples[a] = append(samples[a], []float64{
							float64(c.l1) / 1024,
							float64(c.preads),
							float64(c.blocks),
							float64(c.l1+c.blocks*blockBytes) / 1e6,
						})
					}
				}
				for _, a := range arms {
					report(*label, *docs, a, survive, cl.name, qlen, samples[a])
				}
			}
		}
	}
}

// docFreq is the Zipf-ish docfreq model: the head term hits 45% of
// docs (stopword-class), and df decays as rank^-0.85, which lands
// mid-vocabulary terms in the thousands on a 10M shard.
func docFreq(rank, docs int64) int64 {
	df := int64(0.45 * float64(docs) / math.Pow(float64(rank), 0.85))
	if df < 1 {
		return 1
	}
	return df
}

func mkTerm(rank, docs int64) term {
	df := docFreq(rank, docs)
	return term{rank: rank, df: df, nb: (df + blockLen - 1) / blockLen}
}

// genQueries draws the query set once per (class, qlen) so every arm
// and survive tier prices the identical queries. rand draws ranks Zipf
// over the vocabulary; stop draws from the top 50 ranks only.
func genQueries(class string, qlen, n int, docs, seed int64) [][]term {
	rng := rand.New(rand.NewSource(seed + int64(qlen)))
	zipf := rand.NewZipf(rng, 1.1, 1, vocab-1)
	qs := make([][]term, 0, n)
	for range n {
		seen := map[int64]bool{}
		q := make([]term, 0, qlen)
		for len(q) < qlen {
			var rank int64
			if class == "stop" {
				rank = 1 + rng.Int63n(50)
			} else {
				rank = 1 + int64(zipf.Uint64())
			}
			if seen[rank] {
				continue
			}
			seen[rank] = true
			q = append(q, mkTerm(rank, docs))
		}
		sort.Slice(q, func(i, j int) bool { return q[i].df < q[j].df })
		qs = append(qs, q)
	}
	return qs
}

// simQuery prices one query for all three arms on shared draws;
// returns costs indexed l1only, l1l2, hybrid.
func simQuery(q []term, survive float64, rng *rand.Rand) [3]cost {
	var out [3]cost
	drv := q[0]

	// The driver reads its whole L1 region sequentially and all its
	// blocks; identical in every arm. Sequential preads at 256 KiB.
	driver := cost{
		l1:     pageAlign(drv.nb * l1Bytes),
		preads: (drv.nb*l1Bytes + (256 << 10) - 1) / (256 << 10),
		blocks: drv.nb,
	}
	for i := range out {
		out[i].add(driver)
	}

	for _, t := range q[1:] {
		full, span, blocks := simTerm(t, drv.df, survive, rng)
		hyb := span
		if full.l1 < span.l1 {
			hyb = full
		}
		out[0].add(full)
		out[1].add(span)
		out[2].add(hyb)
		for i := range out {
			out[i].blocks += blocks
		}
	}
	return out
}

// simTerm walks one non-driver term's L1 entries. Each entry is probed
// with the Poissonized probability of holding a driver candidate; a
// probed entry's block survives block-max with probability survive,
// and its L2 span entry passes with probability 1-(1-survive)^32,
// drawn on one uniform so block-pass implies span-pass. Returns the
// l1only cost, the l1l2 cost, and the postings blocks read (identical
// across arms).
func simTerm(t term, drvDF int64, survive float64, rng *rand.Rand) (full, span cost, blocks int64) {
	probeP := 1 - math.Exp(-float64(drvDF)/float64(t.nb))
	spanP := 1 - math.Pow(1-survive, l2Fan)
	var probed int64
	runStart, runEnd := int64(-1), int64(-1)
	flush := func() {
		if runStart < 0 {
			return
		}
		span.l1 += pageAlign((runEnd - runStart + 1) * l1Bytes)
		span.preads++
		runStart, runEnd = -1, -1
	}
	for e := int64(0); e < t.nb; e++ {
		if rng.Float64() >= probeP {
			continue
		}
		probed++
		r := rng.Float64()
		if r < survive {
			blocks++
		}
		if r >= spanP {
			continue
		}
		switch {
		case runStart < 0:
			runStart, runEnd = e, e
		case (e-runEnd)*l1Bytes <= page:
			runEnd = e
		default:
			flush()
			runStart, runEnd = e, e
		}
	}
	flush()
	if probed > 0 {
		// l1only pays the whole array on any probe: nothing resident
		// says the spans would have failed.
		full.l1 = pageAlign(t.nb * l1Bytes)
		full.preads = 1
		if t.rank > knob {
			// L2 not resident for this term: the descent preads the
			// L2 region on any probe, even when every span then
			// fails. The full-array read needs no L2 at all.
			span.l1 += pageAlign(((t.nb + l2Fan - 1) / l2Fan) * l2Bytes)
			span.preads++
		}
	}
	return full, span, blocks
}

// pageAlign rounds bytes up to the 4 KiB pread floor.
func pageAlign(b int64) int64 {
	if b == 0 {
		return 0
	}
	return (b + page - 1) / page * page
}

// pct returns the q-th percentile of sorted samples, nearest-rank.
func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

// report prints one TSV row from per-query samples of
// {l1kb, preads, blocks, nvme_mb}.
func report(label string, docs int64, arm string, survive float64, class string, qlen int, rows [][]float64) {
	cols := make([][]float64, 4)
	for _, r := range rows {
		for i, v := range r {
			cols[i] = append(cols[i], v)
		}
	}
	for i := range cols {
		sort.Float64s(cols[i])
	}
	fmt.Printf("%s\t%d\t%s\t%g\t%s\t%d\t%d\t%.1f\t%.1f\t%.0f\t%.0f\t%.0f\t%.0f\t%.2f\n",
		label, docs, arm, survive, class, qlen, len(rows),
		pct(cols[0], 50), pct(cols[0], 99), pct(cols[1], 50), pct(cols[1], 99),
		pct(cols[2], 50), pct(cols[2], 99), pct(cols[3], 99))
}
