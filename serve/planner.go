// The read planner of doc 07 section 3: Go has no io_uring, so queue
// depth comes from a fixed pool of goroutines in concurrent pread
// against pooled aligned buffers. The traversal hands the planner its
// frontier as a batch of block reads; the planner issues them all
// concurrently and hands buffers back in completion order, so decode
// of arrived blocks overlaps in-flight reads and no traversal step
// ever blocks on one pread.

package serve

import (
	"io"
	"sync"
)

// Planner batch defaults, bound by the labs/c1b/01_readplanner sweep
// on server3 (results/verdict.md). The read unit is the L1-span-sized
// block (P5: 3.3-4.8x the MB/s of 4 KiB at no worse than 1.3x p50).
// Depth 32 is the knee of the measured scaling curve: cold IOPS grow
// 29x from depth 1 to 32 with flat p50, and depth 64 pays 1.54x p50
// for its extra throughput. MaxSpeculative is 0: the quiet re-run
// re-judged the waste arm at +55% time-to-needed p50 for 1x extras
// (the contended +24.5% was a slow baseline hiding them), which fires
// the prediction's conservative branch; speculation waits for a
// governor-driven case. The 3ms cold-batch budget from doc 01's NVMe
// envelope did not survive the virtio disk (P3: 19.6ms p50 for a cold
// 32-block batch); latency budgeting belongs to the governor against
// measured rows, not to a constant here.
const (
	ReadUnit         = 16 << 10
	DefaultReadDepth = 32
	DefaultBatchSize = 32
	MaxSpeculative   = 0
)

// maxOutstanding bounds reads issued but not yet handed back through
// Next. It sizes the planner's channels so Issue never blocks under
// the bound; phase-1 frontiers (~32 blocks) and phase-2 position
// batches (~192) sit far below it.
const maxOutstanding = 1024

// BlockReq is one block the traversal needs: a byte span in the shard
// file plus an opaque tag the caller uses to route the decode.
type BlockReq struct {
	Off int64
	Len int
	Tag uint32
}

// Completion is one finished read. Buf is a pool buffer truncated to
// the bytes read; the caller must hand it back through Release once
// decoded.
type Completion struct {
	Req BlockReq
	Buf []byte
	Err error
}

// PlannerStats count what the planner issued; the traversal lab's
// wasted-read fraction divides consumed by issued.
type PlannerStats struct {
	Reads int64
	Bytes int64
}

// Planner owns a fixed-depth pread pool over one shard file. One
// planner serves one query at a time; a serve worker keeps one per
// mounted generation it is reading and reuses it across queries.
type Planner struct {
	r     io.ReaderAt
	pool  *BufPool
	reqCh chan BlockReq
	outCh chan Completion
	wg    sync.WaitGroup

	inflight int
	stats    PlannerStats
}

// NewPlanner starts depth pread workers over r. Buffers come from
// pool and cap the largest request length.
func NewPlanner(r io.ReaderAt, pool *BufPool, depth int) *Planner {
	if depth <= 0 {
		depth = DefaultReadDepth
	}
	p := &Planner{
		r:     r,
		pool:  pool,
		reqCh: make(chan BlockReq, maxOutstanding),
		outCh: make(chan Completion, maxOutstanding),
	}
	p.wg.Add(depth)
	for range depth {
		go p.worker()
	}
	return p
}

func (p *Planner) worker() {
	defer p.wg.Done()
	for req := range p.reqCh {
		buf := p.pool.Get()
		n, err := p.r.ReadAt(buf[:req.Len], req.Off)
		if err == io.EOF && n == req.Len {
			err = nil
		}
		p.outCh <- Completion{Req: req, Buf: buf[:n:cap(buf)], Err: err}
	}
}

// Issue queues one batch of block reads. It never blocks while total
// outstanding reads stay under maxOutstanding; requests past a
// too-large length complete immediately with an error.
func (p *Planner) Issue(batch []BlockReq) {
	for _, req := range batch {
		if req.Len > p.pool.Size() {
			p.inflight++
			p.outCh <- Completion{Req: req, Err: errBlockTooLarge}
			continue
		}
		p.reqCh <- req
		p.inflight++
		p.stats.Reads++
		p.stats.Bytes += int64(req.Len)
	}
}

// Next returns the next completed read in completion order. It blocks
// while reads are in flight and reports done when nothing is.
func (p *Planner) Next() (Completion, bool) {
	if p.inflight == 0 {
		return Completion{}, false
	}
	c := <-p.outCh
	p.inflight--
	return c, true
}

// Release hands a completion's buffer back to the pool after decode.
func (p *Planner) Release(c Completion) {
	if c.Buf != nil {
		p.pool.Put(c.Buf[:cap(c.Buf)])
	}
}

// Stats reports cumulative issue counts since the planner started.
func (p *Planner) Stats() PlannerStats { return p.stats }

// Close stops the workers after all issued reads drain. The caller
// must consume Next to done before closing.
func (p *Planner) Close() {
	close(p.reqCh)
	p.wg.Wait()
	close(p.outCh)
}

type plannerError string

func (e plannerError) Error() string { return string(e) }

const errBlockTooLarge = plannerError("serve: block read exceeds buffer size")
