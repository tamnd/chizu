// Per-worker allocation discipline, doc 07 section 9: a Worker owns
// every byte the steady query path touches, so serving allocates
// nothing per query once each worker reaches its high-water marks.
// Cursors live in fixed slots with their own aligned block buffers,
// skip regions come from a per-query arena, and the traversal scratch
// (cursor order, bound prefixes, the top-K heap) is reused in place.
// The zero-allocation claim is not prose: TestWorkerZeroAlloc counts.

package serve

import (
	"errors"
	"fmt"

	"github.com/tamnd/chizu/hotfmt"
)

// MaxQueryTerms is the root parse cap (doc 07 section 2): queries
// truncate at 32 terms before they reach a shard.
const MaxQueryTerms = 32

// arenaFloor is the arena's first allocation; big enough that torso
// queries never grow it, small enough to not matter per worker.
const arenaFloor = 64 << 10

// Worker is one query executor's private memory. Not safe for
// concurrent use; the serve loop runs one query per worker at a time.
type Worker struct {
	// Stats accumulates across queries; callers diff snapshots for
	// per-query numbers.
	Stats TraverseStats

	slots []TermCursor
	open  int

	arena []byte
	used  int

	byBound []*TermCursor
	byDocid []*TermCursor
	prefix  []int32
	hits    []Hit
}

// NewWorker sizes a worker for the root parse cap and preallocates
// every fixed-size piece: cursor slots, one aligned block buffer per
// slot, and the traversal scratch.
func NewWorker() *Worker {
	w := &Worker{
		slots:   make([]TermCursor, MaxQueryTerms),
		arena:   make([]byte, arenaFloor),
		byBound: make([]*TermCursor, 0, MaxQueryTerms),
		byDocid: make([]*TermCursor, 0, MaxQueryTerms),
		prefix:  make([]int32, MaxQueryTerms+1),
	}
	for i := range w.slots {
		w.slots[i].buf = alignedBuf(bufAlign)
	}
	return w
}

// Reset starts a new query: slots and the arena rewind, capacity and
// contents stay for reuse.
func (w *Worker) Reset() {
	w.open = 0
	w.used = 0
}

// alloc carves n bytes from the arena. Growth allocates a fresh
// backing (regions handed out earlier this query keep the old one
// alive), which stops once the arena reaches the query mix's
// high-water mark.
func (w *Worker) alloc(n int) []byte {
	if w.used+n > len(w.arena) {
		w.arena = make([]byte, max(2*len(w.arena), n, arenaFloor))
		w.used = 0
	}
	b := w.arena[w.used : w.used+n]
	w.used += n
	return b
}

// OpenTerm looks term up in the mount and binds the next cursor slot
// to it. The false return means the term is absent from the shard; the
// slot is not consumed.
func (w *Worker) OpenTerm(m *Mount, term []byte) (*TermCursor, bool, error) {
	if w.open >= len(w.slots) {
		return nil, false, errors.New("serve: query terms past the root parse cap")
	}
	c := &w.slots[w.open]
	buf := c.buf // survives the slot wipe below
	*c = TermCursor{m: m, stats: &w.Stats, buf: buf, decoded: -1}
	ok, err := m.Dict.LookupInto(term, &c.e, c.inlineArr[:0])
	if err != nil || !ok {
		return nil, ok, err
	}
	if len(c.e.Inline) > 0 {
		c.inline = c.e.Inline
		for _, p := range c.inline {
			c.bound = max(c.bound, p.TF)
		}
		w.open++
		return c, true, nil
	}
	skipsOff, _, err := m.Shard.Band(hotfmt.BandSkips)
	if err != nil {
		return nil, true, err
	}
	postingsOff, _, err := m.Shard.Band(hotfmt.BandPostings)
	if err != nil {
		return nil, true, err
	}
	region := w.alloc(hotfmt.SkipRegionSize(c.e.DF))
	if _, err := m.f.ReadAt(region, int64(skipsOff+c.e.SkipOff)); err != nil {
		return nil, true, fmt.Errorf("serve: skip region pread: %w", err)
	}
	c.skips = region
	c.nb, _ = hotfmt.SkipCounts(c.e.DF)
	c.postingsBase = postingsOff + c.e.PostingsOff
	c.bound = hotfmt.SkipBound(region, c.e.DF)
	w.open++
	return c, true, nil
}

// cursors is the query's open cursor set.
func (w *Worker) cursors() []TermCursor { return w.slots[:w.open] }
