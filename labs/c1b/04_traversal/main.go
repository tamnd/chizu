// Lab traversal (doc 07 sections 3 and 13): MaxScore versus block-max
// WAND versus the hybrid choice table on a fixture query log with
// head, torso, tail, and adversarial all-stopword rows. Re-derives the
// term-count crossover instead of trusting Lucene's or Tantivy's
// corpus, and measures the block survival rate the skip-depth verdict
// flagged as the missing number in the doc 05 section 6 arithmetic.
//
// Like the skip-depth lab this is a counting simulation: rows are a
// deterministic function of the seed, host-independent, no perf claim.
// The comparator across algorithms is work counted in the currencies
// the shard budget spends: postings blocks decoded (NVMe bytes plus
// decode CPU), postings scored (CPU), and resident bound checks
// (free). Both algorithms consult the same block-max bounds, so the
// choice changes visit order, never I/O class (doc 07 section 3).
//
// The model: terms carry Zipf docfreq over N docs; postings are
// docid-sorted with per-posting quantized impacts (u8, upper-bound
// honest: a block bound is the max impact in the block, a term bound
// the max over blocks). Scoring is additive quantized impact, the
// phase-1 shape from doc 08. Every algorithm must return the exact
// top-K of the exhaustive union (rank safety is asserted per query,
// not assumed).
//
// Algorithms:
//
//   - exhaustive: score every posting of every term; the golden top-K
//     and the work ceiling.
//   - maxscore: term partitioning by ascending bound with a shared
//     top-K threshold; essential terms drive, non-essential terms are
//     probed only when the running bound clears the threshold, with
//     block bounds tightening the probe.
//   - bmw: block-max WAND; cursors sorted by docid, pivot by
//     cumulative term bounds, block bounds re-checked at the pivot
//     before any decode.
//
// Output is TSV: label docs algo class qterms queries blocks_p50
// blocks_p99 scored_p50 scored_p99 checks_p50 checks_p99 survive_pct
// safe. survive_pct is decoded blocks over bound-checked blocks, the
// measured pruning strength; safe is 1 when every query's top-K
// matched exhaustive.
package main

import (
	"container/heap"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const (
	blockLen = 128
	vocab    = 1_000_000
)

// topK is the phase-1 candidate count (doc 07 section 2 default 1000);
// a variable so the -k flag and tests can move it.
var topK = 1000

// term is a query term with materialized postings.
type term struct {
	rank    int64
	docids  []int32
	impacts []uint8 // per posting, quantized phase-1 contribution
	blkMax  []uint8 // per 128-block max impact
	bound   uint8   // max over blocks
}

func main() {
	label := flag.String("label", "local", "row label; rows are host-independent counts")
	docs := flag.Int64("docs", 10_000_000, "simulated shard doc count")
	queries := flag.Int("queries", 200, "queries per config")
	seed := flag.Int64("seed", 2107, "rng seed")
	k := flag.Int("k", 1000, "phase-1 candidate count")
	flag.Parse()
	topK = *k

	type class struct {
		name  string
		lo    int64 // rank band the class draws from
		hi    int64
		qlens []int
	}
	classes := []class{
		{"head", 1, 1000, []int{2, 3, 4, 5}},
		{"torso", 1000, 50_000, []int{2, 3, 4, 5}},
		{"tail", 50_000, vocab, []int{2, 3, 4, 5}},
		{"stop", 1, 50, []int{3}},
	}

	fmt.Println("label\tdocs\talgo\tclass\tqterms\tqueries\tblocks_p50\tblocks_p99\tscored_p50\tscored_p99\tchecks_p50\tchecks_p99\tsurvive_pct\tsafe")
	for _, cl := range classes {
		for _, qlen := range cl.qlens {
			rng := rand.New(rand.NewSource(*seed + int64(qlen)*7919 + cl.lo))
			var ms, bw, ex []counters
			safeMS, safeBW := true, true
			for range *queries {
				q := genQuery(rng, cl.lo, cl.hi, qlen, *docs)
				gold, exc := exhaustive(q, *docs)
				msTop, msc := maxscore(q)
				bwTop, bwc := bmw(q)
				if !sameTop(gold, msTop) {
					safeMS = false
				}
				if !sameTop(gold, bwTop) {
					safeBW = false
				}
				ex = append(ex, exc)
				ms = append(ms, msc)
				bw = append(bw, bwc)
			}
			report(*label, *docs, "exhaustive", cl.name, qlen, ex, true)
			report(*label, *docs, "maxscore", cl.name, qlen, ms, safeMS)
			report(*label, *docs, "bmw", cl.name, qlen, bw, safeBW)
		}
	}
}

// docFreq matches the skip-depth lab's Zipf model: the head term hits
// 45% of docs and df decays as rank^-0.85.
func docFreq(rank, docs int64) int64 {
	df := int64(0.45 * float64(docs) / math.Pow(float64(rank), 0.85))
	if df < 1 {
		return 1
	}
	return df
}

// mkTerm materializes a term's postings: df docids drawn as integer
// gaps over the docid space (sorted, unique by construction), impacts
// drawn geometric-ish and scaled by an idf-like factor so rare terms
// contribute more, quantized u8. Deterministic per (seed, rank).
func mkTerm(rank, docs, seed int64) term {
	df := docFreq(rank, docs)
	rng := rand.New(rand.NewSource(seed ^ rank*0x9e3779b9))
	t := term{rank: rank}
	t.docids = make([]int32, 0, df)
	t.impacts = make([]uint8, 0, df)
	// idf-like ceiling: rare terms score up to ~200, stopwords ~40.
	ceil := 40 + 160*math.Log(float64(docs)/float64(df))/math.Log(float64(docs))
	stride := float64(docs) / float64(df)
	pos := 0.0
	for range df {
		pos += stride * (0.5 + rng.Float64())
		d := int64(pos)
		if d >= docs {
			break
		}
		// tf-shaped impact: most postings score low, a few near the
		// term ceiling; this is what makes block-max pruning real.
		f := rng.Float64()
		imp := uint8(1 + f*f*f*(ceil-1))
		t.docids = append(t.docids, int32(d))
		t.impacts = append(t.impacts, imp)
	}
	nb := (len(t.docids) + blockLen - 1) / blockLen
	t.blkMax = make([]uint8, nb)
	for i, imp := range t.impacts {
		b := i / blockLen
		if imp > t.blkMax[b] {
			t.blkMax[b] = imp
		}
		if imp > t.bound {
			t.bound = imp
		}
	}
	return t
}

// termCache reuses materialized postings across queries; ranks repeat
// heavily inside a class band and head-term postings are megabytes.
var termCache = map[int64]term{}

func cachedTerm(rank, docs int64) term {
	if t, ok := termCache[rank]; ok {
		return t
	}
	t := mkTerm(rank, docs, 2107)
	termCache[rank] = t
	return t
}

// genQuery draws qlen distinct ranks from the class band, Zipf-tilted
// inside the band so realistic bands still prefer their own head.
func genQuery(rng *rand.Rand, lo, hi int64, qlen int, docs int64) []term {
	seen := map[int64]bool{}
	q := make([]term, 0, qlen)
	for len(q) < qlen {
		u := rng.Float64()
		rank := lo + int64(float64(hi-lo)*u*u)
		if rank >= hi {
			rank = hi - 1
		}
		if seen[rank] {
			continue
		}
		seen[rank] = true
		q = append(q, cachedTerm(rank, docs))
	}
	return q
}

type counters struct {
	blocks int64 // postings blocks decoded
	scored int64 // postings scored
	checks int64 // resident bound checks (term or block)
}

// hit is one scored candidate.
type hit struct {
	docid int32
	score int32
}

// topHeap is a min-heap of the current top-K by score.
type topHeap []hit

func (h topHeap) Len() int           { return len(h) }
func (h topHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h topHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *topHeap) Push(x any)        { *h = append(*h, x.(hit)) }
func (h *topHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
func (h topHeap) threshold() int32 {
	if len(h) < topK {
		return 0
	}
	return h[0].score
}

func offer(h *topHeap, d int32, s int32) {
	if len(*h) < topK {
		heap.Push(h, hit{d, s})
		return
	}
	if s > (*h)[0].score {
		(*h)[0] = hit{d, s}
		heap.Fix(h, 0)
	}
}

// scoreBuf is the reusable exhaustive accumulator; docids touched per
// query are zeroed selectively afterwards.
var (
	scoreBuf []int32
	touched  []int32
)

// exhaustive scores the full union; golden top-K and the work ceiling.
func exhaustive(q []term, docs int64) ([]hit, counters) {
	var c counters
	if int64(len(scoreBuf)) < docs {
		scoreBuf = make([]int32, docs)
	}
	touched = touched[:0]
	for _, t := range q {
		c.blocks += int64(len(t.blkMax))
		c.scored += int64(len(t.docids))
		for i, d := range t.docids {
			if scoreBuf[d] == 0 {
				touched = append(touched, d)
			}
			scoreBuf[d] += int32(t.impacts[i])
		}
	}
	var h topHeap
	for _, d := range touched {
		offer(&h, d, scoreBuf[d])
		scoreBuf[d] = 0
	}
	return normalize(h), c
}

// cursor walks one term's postings with block-skip support.
type cursor struct {
	t       *term
	i       int // posting index
	decoded int // last block index counted as decoded, -1 none
	c       *counters
}

func (cu *cursor) docid() int32 {
	if cu.i >= len(cu.t.docids) {
		return math.MaxInt32
	}
	return cu.t.docids[cu.i]
}

// advance moves to the first posting with docid >= d, counting each
// newly entered block as decoded.
func (cu *cursor) advance(d int32) {
	ds := cu.t.docids
	lo, hi := cu.i, len(ds)
	for lo < hi {
		mid := (lo + hi) / 2
		if ds[mid] < d {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	cu.i = lo
}

// score reads the current posting's impact; the enclosing block is
// decoded on first touch.
func (cu *cursor) score() int32 {
	b := cu.i / blockLen
	if b != cu.decoded {
		cu.decoded = b
		cu.c.blocks++
	}
	cu.c.scored++
	return int32(cu.t.impacts[cu.i])
}

// blockBound is the resident block-max for the block holding the
// first posting at docid >= d.
func (cu *cursor) blockBound(d int32) int32 {
	cu.c.checks++
	ds := cu.t.docids
	lo, hi := cu.i, len(ds)
	for lo < hi {
		mid := (lo + hi) / 2
		if ds[mid] < d {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(ds) {
		return 0
	}
	return int32(cu.t.blkMax[lo/blockLen])
}

// maxscore is the term-partitioned traversal: essential terms drive
// the union, non-essential terms are probed only when the running
// score plus their bounds clears the threshold.
func maxscore(q []term) ([]hit, counters) {
	var c counters
	ts := make([]term, len(q))
	copy(ts, q)
	sort.Slice(ts, func(i, j int) bool { return ts[i].bound < ts[j].bound })
	cur := make([]*cursor, len(ts))
	for i := range ts {
		cur[i] = &cursor{t: &ts[i], decoded: -1, c: &c}
	}
	// prefix[i] = sum of bounds of terms 0..i-1 (the non-essential
	// candidates when the partition sits at i).
	prefix := make([]int32, len(ts)+1)
	for i := range ts {
		prefix[i+1] = prefix[i] + int32(ts[i].bound)
	}
	var h topHeap
	for {
		theta := h.threshold()
		// Partition: terms below part are non-essential (their
		// combined bounds cannot reach theta alone).
		part := 0
		for part < len(ts) && prefix[part+1] <= theta {
			part++
		}
		if part >= len(ts) {
			break // no essential terms left: no new doc can qualify
		}
		// Next docid among essential cursors.
		d := int32(math.MaxInt32)
		for _, cu := range cur[part:] {
			if cu.docid() < d {
				d = cu.docid()
			}
		}
		if d == math.MaxInt32 {
			break
		}
		var s int32
		for _, cu := range cur[part:] {
			if cu.docid() == d {
				s += cu.score()
				cu.i++
			}
		}
		// Probe non-essential terms from the largest bound down,
		// dropping the doc as soon as even full credit for what is
		// left cannot reach theta.
		for i := part - 1; i >= 0; i-- {
			if s+prefix[i+1] <= theta {
				s = -1
				break
			}
			bb := cur[i].blockBound(d)
			if s+bb+prefix[i] <= theta {
				// Tightening term i's bound to its block max already
				// disqualifies the doc.
				s = -1
				break
			}
			cur[i].advance(d)
			if cur[i].docid() == d {
				s += cur[i].score()
				// Consume the posting so a later partition growth
				// cannot rescore this doc through the same cursor.
				cur[i].i++
			}
		}
		if s > theta {
			offer(&h, d, s)
		}
	}
	return normalize(h), c
}

// bmw is block-max WAND: cursors sorted by docid, pivot chosen by
// cumulative term bounds, block bounds re-checked at the pivot before
// any decode.
func bmw(q []term) ([]hit, counters) {
	var c counters
	ts := make([]term, len(q))
	copy(ts, q)
	cur := make([]*cursor, len(ts))
	for i := range ts {
		cur[i] = &cursor{t: &ts[i], decoded: -1, c: &c}
	}
	var h topHeap
	for {
		sort.Slice(cur, func(i, j int) bool { return cur[i].docid() < cur[j].docid() })
		theta := h.threshold()
		// Pivot: first cursor where the cumulative term bounds exceed
		// theta.
		var acc int32
		pivot := -1
		for i, cu := range cur {
			if cu.docid() == math.MaxInt32 {
				break
			}
			acc += int32(cu.t.bound)
			if acc > theta {
				pivot = i
				break
			}
		}
		if pivot < 0 {
			break
		}
		pd := cur[pivot].docid()
		// Block-max refinement at the pivot docid. Cursors beyond the
		// pivot that already sit exactly at pd contribute too; missing
		// them makes the skip unsafe.
		var bacc int32
		for i, cu := range cur {
			if i > pivot && cu.docid() != pd {
				break
			}
			bacc += cu.blockBound(pd)
		}
		if bacc <= theta {
			// The pivot's blocks cannot qualify: advance the leading
			// cursor past its current block or to the pivot.
			cu := cur[0]
			next := int32(math.MaxInt32)
			if b := cu.i/blockLen + 1; b < len(cu.t.blkMax) {
				next = cu.t.docids[b*blockLen]
			}
			if pd+1 < next {
				next = pd + 1
			}
			cu.advance(next)
			continue
		}
		if cur[0].docid() == pd {
			var s int32
			for _, cu := range cur {
				if cu.docid() != pd {
					break
				}
				s += cu.score()
				cu.i++
			}
			if s > theta {
				offer(&h, pd, s)
			}
		} else {
			cur[0].advance(pd)
		}
	}
	return normalize(h), c
}

// normalize sorts a heap's contents into a canonical comparable form:
// by score descending, docid ascending.
func normalize(h topHeap) []hit {
	out := make([]hit, len(h))
	copy(out, h)
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].docid < out[j].docid
	})
	return out
}

// sameTop compares score sequences (rank safety is about scores; ties
// may legally resolve to different docids only if scores match).
func sameTop(gold, got []hit) bool {
	if len(gold) != len(got) {
		return false
	}
	for i := range gold {
		if gold[i].score != got[i].score {
			return false
		}
	}
	return true
}

// pct returns the q-th percentile of sorted samples, nearest-rank.
func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func report(label string, docs int64, algo, class string, qlen int, cs []counters, safe bool) {
	var blocks, scored, checks []float64
	var decSum, chkSum int64
	for _, c := range cs {
		blocks = append(blocks, float64(c.blocks))
		scored = append(scored, float64(c.scored))
		checks = append(checks, float64(c.checks))
		decSum += c.blocks
		chkSum += c.checks
	}
	sort.Float64s(blocks)
	sort.Float64s(scored)
	sort.Float64s(checks)
	surv := 100.0
	if chkSum > 0 {
		surv = 100 * float64(decSum) / float64(chkSum)
	}
	safeN := 0
	if safe {
		safeN = 1
	}
	fmt.Printf("%s\t%d\t%s\t%s\t%d\t%d\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.1f\t%d\n",
		label, docs, algo, class, qlen, len(cs),
		pct(blocks, 50), pct(blocks, 99), pct(scored, 50), pct(scored, 99),
		pct(checks, 50), pct(checks, 99), surv, safeN)
}
