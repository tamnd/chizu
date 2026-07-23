// Phase-1 traversal, doc 07 section 3: MaxScore and block-max WAND
// over term cursors with a shared top-K heap, flat quantized scores
// this milestone (the score is the sum of per-posting u8 impacts; BM25F
// weighting arrives with doc 08 at C3). Both algorithms consult the
// same resident block-max bounds, so the choice changes visit order,
// never I/O class. Rank safety is exact: every traversal returns the
// true top-K score set of the term union.

package serve

import "sort"

// bmwMaxTerms is the hybrid crossover measured by lab 04 (PR #53): BMW
// wins blocks decoded and postings scored at every query length but
// pays up to 9x the bound checks, and the check CPU overtakes the
// decode savings past two terms, so BMW drives 1-2 term queries and
// MaxScore takes 3+.
const bmwMaxTerms = 2

// Hit is one phase-1 candidate.
type Hit struct {
	Docid uint32
	Score int32
}

// Traverse runs the hybrid choice table over the cursors: block-max
// WAND at bmwMaxTerms and below, MaxScore above. deleted is the doc 07
// exclusion check (tombstones); nil means nothing is excluded. Hits
// come back score-descending, docid-ascending.
func Traverse(cur []*TermCursor, k int, deleted func(uint32) bool) ([]Hit, error) {
	if len(cur) <= bmwMaxTerms {
		return TraverseBMW(cur, k, deleted)
	}
	return TraverseMaxScore(cur, k, deleted)
}

// TraverseMaxScore is the term-partitioned traversal: terms sort by
// ascending bound, the essential suffix (whose bounds alone can beat
// the threshold) drives the docid union, and non-essential terms are
// probed from the largest bound down with block-max tightening, so a
// doc drops as soon as the credit left cannot reach the threshold.
func TraverseMaxScore(cur []*TermCursor, k int, deleted func(uint32) bool) ([]Hit, error) {
	ts := make([]*TermCursor, len(cur))
	copy(ts, cur)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Bound() < ts[j].Bound() })
	// prefix[i] is the summed bound of terms 0..i-1, the most the
	// non-essential side can add when the partition sits at i.
	prefix := make([]int32, len(ts)+1)
	for i, c := range ts {
		prefix[i+1] = prefix[i] + int32(c.Bound())
	}
	h := topHeap{k: k}
	for {
		theta := h.threshold()
		part := 0
		for part < len(ts) && prefix[part+1] <= theta {
			part++
		}
		if part >= len(ts) {
			break // no essential terms left: no new doc can qualify
		}
		d := docidExhausted
		for _, c := range ts[part:] {
			d = min(d, c.Docid())
		}
		if d == docidExhausted {
			break
		}
		var s int32
		for _, c := range ts[part:] {
			if c.Docid() == d {
				s += int32(c.Impact())
				c.Next()
			}
		}
		for i := part - 1; i >= 0; i-- {
			if s+prefix[i+1] <= theta {
				s = -1
				break
			}
			bb := int32(ts[i].BlockBound(d))
			if s+bb+prefix[i] <= theta {
				// Tightening term i's bound to its block max already
				// disqualifies the doc; its block is never read.
				s = -1
				break
			}
			ts[i].Advance(d)
			if ts[i].Docid() == d {
				s += int32(ts[i].Impact())
				// Consume the posting so a later partition growth
				// cannot rescore this doc through the same cursor.
				ts[i].Next()
			}
		}
		if s > theta && (deleted == nil || !deleted(d)) {
			h.offer(d, s)
		}
	}
	return h.sorted(), cursorErr(ts)
}

// TraverseBMW is block-max WAND: cursors sort by docid, the pivot is
// the first prefix whose term bounds beat the threshold, and the
// pivot's block bounds are re-checked before any decode; a rejected
// pivot advances the leading cursor past its block or to the pivot,
// whichever is nearer.
func TraverseBMW(cur []*TermCursor, k int, deleted func(uint32) bool) ([]Hit, error) {
	ts := make([]*TermCursor, len(cur))
	copy(ts, cur)
	h := topHeap{k: k}
	for {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Docid() < ts[j].Docid() })
		theta := h.threshold()
		var acc int32
		pivot := -1
		for i, c := range ts {
			if c.Docid() == docidExhausted {
				break
			}
			acc += int32(c.Bound())
			if acc > theta {
				pivot = i
				break
			}
		}
		if pivot < 0 {
			break
		}
		pd := ts[pivot].Docid()
		// Block-max refinement at the pivot docid. Cursors past the
		// pivot that already sit exactly at pd contribute too; missing
		// them makes the skip unsafe.
		var bacc int32
		for i, c := range ts {
			if i > pivot && c.Docid() != pd {
				break
			}
			bacc += int32(c.BlockBound(pd))
		}
		if bacc <= theta {
			// The pivot's blocks cannot qualify: move the leading
			// cursor without decoding what the bound just killed.
			ts[0].Advance(min(ts[0].shallowNext(), pd+1))
			continue
		}
		if ts[0].Docid() == pd {
			var s int32
			for _, c := range ts {
				if c.Docid() != pd {
					break
				}
				s += int32(c.Impact())
				c.Next()
			}
			if s > theta && (deleted == nil || !deleted(pd)) {
				h.offer(pd, s)
			}
		} else {
			ts[0].Advance(pd)
		}
	}
	return h.sorted(), cursorErr(ts)
}

// cursorErr surfaces the first sticky cursor error; a failed cursor
// reads as exhausted so the loops above never see a torn state.
func cursorErr(ts []*TermCursor) error {
	for _, c := range ts {
		if c.Err() != nil {
			return c.Err()
		}
	}
	return nil
}

// topHeap is a bounded min-heap of the best k hits by score; its floor
// is the shared threshold both traversals prune against.
type topHeap struct {
	k    int
	hits []Hit
}

// threshold is the score a new doc must beat to enter the heap, zero
// until the heap fills.
func (h *topHeap) threshold() int32 {
	if len(h.hits) < h.k {
		return 0
	}
	return h.hits[0].Score
}

func (h *topHeap) offer(d uint32, s int32) {
	if len(h.hits) < h.k {
		h.hits = append(h.hits, Hit{d, s})
		i := len(h.hits) - 1
		for i > 0 {
			p := (i - 1) / 2
			if h.hits[p].Score <= h.hits[i].Score {
				break
			}
			h.hits[p], h.hits[i] = h.hits[i], h.hits[p]
			i = p
		}
		return
	}
	if s <= h.hits[0].Score {
		return
	}
	h.hits[0] = Hit{d, s}
	i := 0
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < len(h.hits) && h.hits[l].Score < h.hits[m].Score {
			m = l
		}
		if r < len(h.hits) && h.hits[r].Score < h.hits[m].Score {
			m = r
		}
		if m == i {
			break
		}
		h.hits[i], h.hits[m] = h.hits[m], h.hits[i]
		i = m
	}
}

// sorted drains the heap into the canonical order: score descending,
// docid ascending.
func (h *topHeap) sorted() []Hit {
	out := h.hits
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Docid < out[j].Docid
	})
	return out
}
