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
//     array (sequential, identical in both arms). Every other term is
//     probed at each driver candidate docid; no block-max pruning is
//     modeled, so rows are the worst case section 6 budgets against,
//     and pruning only shrinks both arms.
//   - l1only: a probed term preads its entire L1 array, because
//     nothing resident says where to look.
//   - l1l2: resident L2 names the L1 spans, so the term preads only
//     the touched L1 runs (runs closer than 4 KiB coalesce). Terms
//     outside the residency knob (top-knob df ranks keep L2 mlocked)
//     pay one extra pread for their L2 region first.
//
// Output is TSV: label arm knob class qterms queries l1kb_p50 l1kb_p99
// preads_p50 preads_p99 blocks_p50 blocks_p99 nvme_mb_p99. l1kb is
// skip-band bytes pread; nvme_mb adds postings blocks at the doc 05
// compression accounting (~1.3 KB per block).
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const (
	blockLen    = 128       // postings per block
	l1Bytes     = 15        // L1 entry: 10 skip + 5 positions offset
	l2Bytes     = 12        // padded L2 entry
	l2Fan       = 32        // L1 entries per L2 entry
	blockBytes  = 1331      // ~1.3 KB compressed postings block (doc 05 section 6)
	coalesceGap = 4096      // L1 runs closer than one page read as one pread
	vocab       = 1_000_000 // term ranks drawn 1..vocab
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
	type arm struct {
		name string
		knob int64 // top-knob df ranks keep L2 resident; 0 = no resident L2
	}
	arms := []arm{
		{"l1only", 0},
		{"l1l2", 0},
		{"l1l2", 1000},
		{"l1l2", 10000},
	}

	fmt.Println("label\tarm\tknob\tclass\tqterms\tqueries\tl1kb_p50\tl1kb_p99\tpreads_p50\tpreads_p99\tblocks_p50\tblocks_p99\tnvme_mb_p99")
	for _, cl := range classes {
		for _, qlen := range cl.qlens {
			qs := genQueries(cl.name, qlen, *queries, *docs, *seed)
			for _, a := range arms {
				var l1kb, preads, blocks, mb []float64
				rng := rand.New(rand.NewSource(*seed + 1))
				for _, q := range qs {
					c := simQuery(q, *docs, a.name, a.knob, rng)
					l1kb = append(l1kb, float64(c.l1)/1024)
					preads = append(preads, float64(c.preads))
					blocks = append(blocks, float64(c.blocks))
					mb = append(mb, float64(c.l1+c.blocks*blockBytes)/1e6)
				}
				sort.Float64s(l1kb)
				sort.Float64s(preads)
				sort.Float64s(blocks)
				sort.Float64s(mb)
				fmt.Printf("%s\t%s\t%d\t%s\t%d\t%d\t%.1f\t%.1f\t%.0f\t%.0f\t%.0f\t%.0f\t%.2f\n",
					*label, a.name, a.knob, cl.name, qlen, len(qs),
					pct(l1kb, 50), pct(l1kb, 99), pct(preads, 50), pct(preads, 99),
					pct(blocks, 50), pct(blocks, 99), pct(mb, 99))
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
// prices the identical queries. rand draws ranks Zipf over the
// vocabulary; stop draws from the top 50 ranks only.
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

// simQuery walks one query: the rarest term drives, every candidate
// docid probes the other terms, and the arm decides what the skip
// descent costs.
func simQuery(q []term, docs int64, arm string, knob int64, rng *rand.Rand) cost {
	var c cost
	drv := q[0]

	// The driver reads its whole L1 region sequentially and all its
	// blocks; identical in both arms. Sequential preads at 256 KiB.
	c.l1 += drv.nb * l1Bytes
	c.preads += (drv.nb*l1Bytes + (256 << 10) - 1) / (256 << 10)
	c.blocks += drv.nb

	// Candidate docids: a uniform draw of df_driver positions over the
	// docid space, generated in order as exponential gaps.
	for _, t := range q[1:] {
		mean := float64(docs) / float64(drv.df)
		var probes, runBytes, runs int64
		runStart, runEnd, last := int64(-1), int64(-1), int64(-1)
		docid := 0.0
		for {
			docid += rng.ExpFloat64() * mean
			d := int64(docid)
			if d >= docs {
				break
			}
			blk := d * t.nb / docs
			if blk == last {
				continue
			}
			last = blk
			probes++
			switch {
			case runStart < 0:
				runStart, runEnd = blk, blk
			case (blk-runEnd)*l1Bytes <= coalesceGap:
				runEnd = blk
			default:
				runBytes += (runEnd - runStart + 1) * l1Bytes
				runs++
				runStart, runEnd = blk, blk
			}
		}
		if runStart >= 0 {
			runBytes += (runEnd - runStart + 1) * l1Bytes
			runs++
		}
		switch arm {
		case "l1only":
			// No resident level says where to look: the whole L1
			// array comes in as one read.
			c.l1 += t.nb * l1Bytes
			c.preads++
		case "l1l2":
			c.l1 += runBytes
			c.preads += runs
			if t.rank > knob && probes > 0 {
				// L2 not resident for this term: one pread for its
				// L2 region before the descent.
				c.l1 += ((t.nb + l2Fan - 1) / l2Fan) * l2Bytes
				c.preads++
			}
		}
		c.blocks += probes
	}
	return c
}

// pct returns the q-th percentile of sorted samples, nearest-rank.
func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}
