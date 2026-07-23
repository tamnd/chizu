// Term cursors, doc 07 section 3: a cursor walks one term's postings
// in docid order over the doc 05 skip hierarchy. The resident L1 array
// answers docid targeting and block-max bounds without touching the
// postings band; a block's bytes are pread through the pool and
// decoded only when the traversal actually positions inside it, so the
// blocks a bound check kills are never read. This slice fetches blocks
// synchronously; the read planner slice batches the preads without
// changing this contract.

package serve

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/tamnd/chizu/hotfmt"
)

// docidExhausted orders exhausted cursors after every live one. Real
// docids are shard-local and bounded by the doc count, far below the
// u32 ceiling.
const docidExhausted = ^uint32(0)

// TraverseStats counts traversal work in the currencies the shard
// budget is priced in (lab 04's comparator): blocks decoded cost NVMe
// bytes plus decode CPU, scored postings cost CPU, bound checks are
// resident and free.
type TraverseStats struct {
	Blocks int64 // postings blocks pread and decoded
	Scored int64 // postings scored into the heap's candidate stream
	Checks int64 // resident block-max bound checks
}

// TermCursor is one term's docid-ordered walk. Position moves only
// forward; Docid decodes the block it lands in on demand. I/O and
// format errors stick: the cursor reports exhausted and Err carries
// the cause, so traversal loops stay branch-light.
type TermCursor struct {
	m     *Mount
	pool  *BufPool
	stats *TraverseStats
	err   error

	e            hotfmt.DictEntry
	l1           []hotfmt.SkipL1
	inline       []hotfmt.InlinePosting
	postingsBase uint64
	bound        uint8

	blk     int // current block index in l1
	i       int // position inside the decoded block or the inline list
	decoded int // block index the buffers hold, -1 before the first read
	n       int // entries in the decoded block
	done    bool
	docids  [hotfmt.PostingsBlockLen]uint32
	impacts [hotfmt.PostingsBlockLen]uint8
	masks   [hotfmt.PostingsBlockLen]uint8
}

// OpenTerm looks the term up and builds its cursor: inline entries
// carry their postings in the dictionary, everything else preads its
// skip region once and keeps the L1 array for the walk. The second
// return is false when the term is absent from the shard.
func (m *Mount) OpenTerm(term []byte, pool *BufPool, stats *TraverseStats) (*TermCursor, bool, error) {
	e, ok, err := m.Dict.Lookup(term)
	if err != nil || !ok {
		return nil, ok, err
	}
	c := &TermCursor{m: m, pool: pool, stats: stats, e: e, decoded: -1}
	if len(e.Inline) > 0 {
		c.inline = e.Inline
		for _, p := range e.Inline {
			c.bound = max(c.bound, p.TF)
		}
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
	region := make([]byte, hotfmt.SkipRegionSize(e.DF))
	if _, err := m.f.ReadAt(region, int64(skipsOff+e.SkipOff)); err != nil {
		return nil, true, fmt.Errorf("serve: skip region pread: %w", err)
	}
	l1, _, _, err := hotfmt.ParseSkips(region, e.DF)
	if err != nil {
		return nil, true, err
	}
	c.l1 = l1
	c.postingsBase = postingsOff + e.PostingsOff
	for _, s := range l1 {
		c.bound = max(c.bound, s.Impact)
	}
	return c, true, nil
}

// Bound is the term's max quantized impact, the MaxScore partitioning
// and WAND pivot currency.
func (c *TermCursor) Bound() uint8 { return c.bound }

// DF is the term's document frequency.
func (c *TermCursor) DF() uint32 { return c.e.DF }

// Err reports the sticky I/O or format error, if any.
func (c *TermCursor) Err() error { return c.err }

// fail records the first error and exhausts the cursor.
func (c *TermCursor) fail(err error) {
	if c.err == nil {
		c.err = err
	}
	c.done = true
}

// ensure decodes the current block if the buffers hold another one.
func (c *TermCursor) ensure() bool {
	if c.done {
		return false
	}
	if c.inline != nil || c.decoded == c.blk {
		return true
	}
	end := uint64(c.e.PostingsLen)
	if c.blk+1 < len(c.l1) {
		end = c.l1[c.blk+1].Off
	}
	length := int(end - c.l1[c.blk].Off)
	if length <= 0 || length > c.pool.Size() {
		c.fail(fmt.Errorf("serve: postings block of %d bytes", length))
		return false
	}
	buf := c.pool.Get()
	n, err := c.m.f.ReadAt(buf[:length], int64(c.postingsBase+c.l1[c.blk].Off))
	if err == io.EOF && n == length {
		err = nil
	}
	if err != nil {
		c.pool.Put(buf)
		c.fail(fmt.Errorf("serve: postings block pread: %w", err))
		return false
	}
	prev := int64(-1)
	if c.blk > 0 {
		prev = int64(c.l1[c.blk-1].LastDocid)
	}
	b, used, err := hotfmt.DecodePostingsBlock(buf[:length], prev, c.docids[:], c.impacts[:], c.masks[:])
	c.pool.Put(buf)
	if err != nil {
		c.fail(err)
		return false
	}
	if used != length || b.LastDocid != c.l1[c.blk].LastDocid {
		c.fail(errors.New("serve: postings block disagrees with its skip entry"))
		return false
	}
	c.n = b.NEntries
	c.decoded = c.blk
	c.stats.Blocks++
	return true
}

// Docid is the current posting's docid, docidExhausted past the end.
func (c *TermCursor) Docid() uint32 {
	if !c.ensure() {
		return docidExhausted
	}
	if c.inline != nil {
		return c.inline[c.i].Docid
	}
	return c.docids[c.i]
}

// Impact scores the current posting; the caller is positioned (the
// last Docid returned a real docid).
func (c *TermCursor) Impact() uint8 {
	c.stats.Scored++
	if c.inline != nil {
		return c.inline[c.i].TF
	}
	return c.impacts[c.i]
}

// Next steps to the following posting.
func (c *TermCursor) Next() {
	if c.done {
		return
	}
	c.i++
	if c.inline != nil {
		if c.i >= len(c.inline) {
			c.done = true
		}
		return
	}
	if c.decoded == c.blk && c.i >= c.n {
		c.blk++
		c.i = 0
		if c.blk >= len(c.l1) {
			c.done = true
		}
	}
}

// Advance moves to the first posting with docid >= d. Blocks between
// the current position and the target are skipped by the L1 array
// without a read; only the landed block decodes.
func (c *TermCursor) Advance(d uint32) {
	if c.done {
		return
	}
	if c.inline != nil {
		for c.i < len(c.inline) && c.inline[c.i].Docid < d {
			c.i++
		}
		if c.i >= len(c.inline) {
			c.done = true
		}
		return
	}
	t := c.blk + sort.Search(len(c.l1)-c.blk, func(j int) bool {
		return c.l1[c.blk+j].LastDocid >= d
	})
	if t >= len(c.l1) {
		c.done = true
		return
	}
	if t != c.blk {
		c.blk = t
		c.i = 0
	}
	if !c.ensure() {
		return
	}
	lo, hi := c.i, c.n
	for lo < hi {
		mid := (lo + hi) / 2
		if c.docids[mid] < d {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	c.i = lo
}

// BlockBound is the resident block-max for the block holding the first
// posting at docid >= d, zero when the term is exhausted before d. No
// postings byte moves: this is the check that kills blocks before
// their bytes are read.
func (c *TermCursor) BlockBound(d uint32) uint8 {
	c.stats.Checks++
	if c.done {
		return 0
	}
	if c.inline != nil {
		var m uint8
		for _, p := range c.inline[c.i:] {
			if p.Docid >= d {
				m = max(m, p.TF)
			}
		}
		return m
	}
	t := c.blk + sort.Search(len(c.l1)-c.blk, func(j int) bool {
		return c.l1[c.blk+j].LastDocid >= d
	})
	if t >= len(c.l1) {
		return 0
	}
	return c.l1[t].Impact
}

// shallowNext is the first docid past the cursor's current block, the
// block-max WAND move that steps over a rejected block without
// decoding it. Inline cursors have no blocks to step over.
func (c *TermCursor) shallowNext() uint32 {
	if c.done || c.inline != nil {
		return docidExhausted
	}
	return c.l1[c.blk].LastDocid + 1
}
