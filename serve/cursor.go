// Term cursors, doc 07 section 3: a cursor walks one term's postings
// in docid order over the doc 05 skip hierarchy. The raw skip region
// (arena bytes, read once at OpenTerm) answers docid targeting and
// block-max bounds without touching the postings band; a block's bytes
// are pread into the cursor's own aligned buffer and decoded only when
// the traversal actually positions inside it, so the blocks a bound
// check kills are never read. Cursors live in Worker slots and hold no
// heap allocations of their own (doc 07 section 9). This slice fetches
// blocks synchronously; the read planner slice batches the preads
// without changing this contract.

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
	stats *TraverseStats
	buf   []byte // slot-owned aligned pread buffer
	err   error

	e            hotfmt.DictEntry
	inlineArr    [4]hotfmt.InlinePosting // backs e.Inline for df<=4 terms
	skips        []byte                  // raw skip region in the worker arena
	nb           int                     // L1 entry count in skips
	inline       []hotfmt.InlinePosting
	postingsBase uint64
	bound        uint8

	blk     int // current block index
	i       int // position inside the decoded block or the inline list
	decoded int // block index the buffers hold, -1 before the first read
	n       int // entries in the decoded block
	done    bool
	docids  [hotfmt.PostingsBlockLen]uint32
	impacts [hotfmt.PostingsBlockLen]uint8
	masks   [hotfmt.PostingsBlockLen]uint8
}

// l1 reads L1 entry j straight from the raw region.
func (c *TermCursor) l1(j int) hotfmt.SkipL1 { return hotfmt.SkipL1At(c.skips, j) }

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
// The lastDocid cross-check against the skip entry is the lazy
// validation of the raw region: OpenTerm no longer parses it whole.
func (c *TermCursor) ensure() bool {
	if c.done {
		return false
	}
	if c.inline != nil || c.decoded == c.blk {
		return true
	}
	cur := c.l1(c.blk)
	end := uint64(c.e.PostingsLen)
	if c.blk+1 < c.nb {
		end = c.l1(c.blk + 1).Off
	}
	length := int(end - cur.Off)
	if length <= 0 || length > len(c.buf) {
		c.fail(fmt.Errorf("serve: postings block of %d bytes", length))
		return false
	}
	n, err := c.m.f.ReadAt(c.buf[:length], int64(c.postingsBase+cur.Off))
	if err == io.EOF && n == length {
		err = nil
	}
	if err != nil {
		c.fail(fmt.Errorf("serve: postings block pread: %w", err))
		return false
	}
	prev := int64(-1)
	if c.blk > 0 {
		prev = int64(c.l1(c.blk - 1).LastDocid)
	}
	b, used, err := hotfmt.DecodePostingsBlock(c.buf[:length], prev, c.docids[:], c.impacts[:], c.masks[:])
	if err != nil {
		c.fail(err)
		return false
	}
	if used != length || b.LastDocid != cur.LastDocid {
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
		if c.blk >= c.nb {
			c.done = true
		}
	}
}

// Advance moves to the first posting with docid >= d. Blocks between
// the current position and the target are skipped by the L1 entries
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
	t := c.blk + sort.Search(c.nb-c.blk, func(j int) bool {
		return c.l1(c.blk+j).LastDocid >= d
	})
	if t >= c.nb {
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
	t := c.blk + sort.Search(c.nb-c.blk, func(j int) bool {
		return c.l1(c.blk+j).LastDocid >= d
	})
	if t >= c.nb {
		return 0
	}
	return c.l1(t).Impact
}

// shallowNext is the first docid past the cursor's current block, the
// block-max WAND move that steps over a rejected block without
// decoding it. Inline cursors have no blocks to step over.
func (c *TermCursor) shallowNext() uint32 {
	if c.done || c.inline != nil {
		return docidExhausted
	}
	return c.l1(c.blk).LastDocid + 1
}
