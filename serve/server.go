// The single-node serve loop, doc 07: QUERY frames in over the doc 02
// wire, phase-1 execution against the mounted generation set, QRESULT
// out before the deadline. The loop's job is dispatch and honesty: a
// node over its in-flight cap answers BUSY immediately (the root's
// instant hedge trigger), a CANCEL kills the loser of a hedge by
// reqid, and the deadline re-anchors on this node's monotonic clock at
// receipt. Deadline enforcement this slice is at shard boundaries; the
// governor slice wires T1/T2/T3 into the traversal loops themselves.

package serve

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/chizu/wire"
)

// Degrade bits, the doc 07 section 5 taxonomy. The serve loop sets
// DegradeShard when budget exhaustion stops it before a listed shard;
// the finer bits arrive with the governor slice.
const (
	DegradeTerms   byte = 1 << 0 // dropped optional terms
	DegradeK       byte = 1 << 1 // reduced K
	DegradeBlocks  byte = 1 << 2 // budget-stopped traversal
	DegradeReplica byte = 1 << 3 // hedged answer used (set by the root)
	DegradeShard   byte = 1 << 4 // a shard missed entirely
)

// t3Fraction is the serialization milestone: the response goes out at
// 95% of budget no matter what remains undone (doc 07 section 5).
const t3Fraction = 0.95

// ServerOptions sizes a serve node.
type ServerOptions struct {
	NodeID      uint64
	MaxInflight int // concurrent queries before BUSY; 0 means 64
}

// Server owns the accept loop, the worker pool, and the in-flight
// accounting for one serve node.
type Server struct {
	reg      *Registry
	nodeID   uint64
	cap      int32
	inflight atomic.Int32
	workers  chan *Worker

	mu       sync.Mutex
	closed   bool
	listener net.Listener
	conns    map[net.Conn]struct{}
}

// NewServer builds a server over the registry's mounted shards.
// Workers are created up front, one per in-flight slot, so the query
// path never constructs one.
func NewServer(reg *Registry, opts ServerOptions) *Server {
	n := opts.MaxInflight
	if n <= 0 {
		n = 64
	}
	s := &Server{
		reg:     reg,
		nodeID:  opts.NodeID,
		cap:     int32(n),
		workers: make(chan *Worker, n),
		conns:   make(map[net.Conn]struct{}),
	}
	for range n {
		s.workers <- NewWorker()
	}
	return s
}

// Serve accepts connections until the listener closes. Each connection
// runs its own reader loop; responses interleave under a per-connection
// write lock.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("serve: server closed")
	}
	s.listener = l
	s.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return nil
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

// Close stops accepting, closes every live connection, and lets
// in-flight queries finish against their pinned generations.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	l := s.listener
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	var err error
	if l != nil {
		err = l.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return err
}

// conn is one live connection's shared state: the write lock that
// serializes response frames and the cancel set keyed by reqid.
type conn struct {
	s  *Server
	c  net.Conn
	wm sync.Mutex

	cm        sync.Mutex
	cancelled map[uint64]struct{}
	live      map[uint64]struct{}
}

func (s *Server) serveConn(nc net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, nc)
		s.mu.Unlock()
		_ = nc.Close()
	}()
	cn := &conn{
		s:         s,
		c:         nc,
		cancelled: map[uint64]struct{}{},
		live:      map[uint64]struct{}{},
	}
	fr := wire.NewFrameReader(nc)

	// The connection opens with a HELLO exchange: the peer declares
	// itself, the node answers with its own limits.
	f, err := fr.Next()
	if err != nil || f.Kind != wire.KindHello {
		return
	}
	if _, err := wire.ParseHello(f.Body); err != nil {
		return
	}
	hello, err := wire.AppendHello(nil, &wire.Hello{
		Version:     1,
		NodeID:      s.nodeID,
		MaxInflight: uint32(s.cap),
		MaxFrame:    wire.MaxFrame,
	})
	if err != nil {
		return
	}
	if err := cn.write(wire.KindHello, f.Reqid, hello); err != nil {
		return
	}

	for {
		f, err := fr.Next()
		if err != nil {
			return
		}
		switch f.Kind {
		case wire.KindPing:
			if err := cn.write(wire.KindPing, f.Reqid, nil); err != nil {
				return
			}
		case wire.KindCancel:
			cn.cancel(f.Reqid)
		case wire.KindQuery:
			q, err := wire.ParseQuery(f.Body)
			if err != nil {
				return // malformed frames close the connection (doc 07 section 1)
			}
			if !s.admit() {
				if err := cn.write(wire.KindBusy, f.Reqid, nil); err != nil {
					return
				}
				continue
			}
			// The parsed query aliases the reader's buffer, which the
			// next Next() overwrites: copy it before the goroutine hop.
			q = cloneQuery(q)
			// Re-anchor before the hop so queueing delay spends the
			// query's own budget, not free time.
			deadline := wire.Reanchor(time.Now(), q.BudgetUS)
			cn.begin(f.Reqid)
			go func(reqid uint64) {
				defer s.release()
				w := <-s.workers
				cn.execute(reqid, q, deadline, w)
				s.workers <- w
			}(f.Reqid)
		default:
			return // unknown or role-reversed kinds are protocol errors
		}
	}
}

// cloneQuery deep-copies a query out of the frame reader's buffer.
func cloneQuery(q *wire.Query) *wire.Query {
	out := *q
	out.Shards = append([]uint16(nil), q.Shards...)
	out.Terms = make([]wire.QueryTerm, len(q.Terms))
	for i, t := range q.Terms {
		out.Terms[i] = wire.QueryTerm{
			Term:   append([]byte(nil), t.Term...),
			DFHint: t.DFHint,
			Op:     t.Op,
		}
	}
	return &out
}

// admit reserves an in-flight slot, refusing over the cap: a queued
// query that will miss its deadline is worse than a fast no.
func (s *Server) admit() bool {
	if s.inflight.Add(1) > s.cap {
		s.inflight.Add(-1)
		return false
	}
	return true
}

func (s *Server) release() { s.inflight.Add(-1) }

func (cn *conn) write(kind byte, reqid uint64, body []byte) error {
	frame, err := wire.AppendFrame(nil, kind, reqid, body)
	if err != nil {
		return err
	}
	cn.wm.Lock()
	defer cn.wm.Unlock()
	_, err = cn.c.Write(frame)
	return err
}

// begin registers a live reqid so a CANCEL arriving mid-execution can
// find it.
func (cn *conn) begin(reqid uint64) {
	cn.cm.Lock()
	cn.live[reqid] = struct{}{}
	cn.cm.Unlock()
}

// cancel marks a reqid dead. A cancel for an unknown reqid is
// remembered briefly via the live set being empty: it simply drops,
// which is safe because the root only cancels reqids it issued.
func (cn *conn) cancel(reqid uint64) {
	cn.cm.Lock()
	if _, ok := cn.live[reqid]; ok {
		cn.cancelled[reqid] = struct{}{}
	}
	cn.cm.Unlock()
}

// finish removes the reqid and reports whether it was cancelled while
// running; a cancelled query writes nothing.
func (cn *conn) finish(reqid uint64) (cancelled bool) {
	cn.cm.Lock()
	_, cancelled = cn.cancelled[reqid]
	delete(cn.cancelled, reqid)
	delete(cn.live, reqid)
	cn.cm.Unlock()
	return cancelled
}

// execute runs phase 1 for every listed shard and writes one QRESULT.
// Budget checks sit at shard boundaries this slice: a shard the budget
// cannot reach is skipped with DegradeShard set, and serialization
// happens by T3 because the last boundary check reserves the write.
func (cn *conn) execute(reqid uint64, q *wire.Query, deadline time.Time, w *Worker) {
	budget := time.Duration(q.BudgetUS) * time.Microsecond
	t3 := deadline.Add(-time.Duration(float64(budget) * (1 - t3Fraction)))
	var degrade byte
	entries := make([]wire.ResultEntry, 0, int(q.K))

	for _, shard := range q.Shards {
		if !time.Now().Before(t3) {
			degrade |= DegradeShard
			continue
		}
		hits, err := cn.executeShard(q, shard, w)
		if err != nil {
			degrade |= DegradeShard
			continue
		}
		entries = mergeHits(entries, hits, shard, int(q.K))
	}

	if cn.finish(reqid) {
		return
	}
	body, err := wire.AppendQResult(nil, &wire.QResult{Degrade: degrade, Entries: entries})
	if err != nil {
		return
	}
	_ = cn.write(wire.KindQResult, reqid, body)
}

// executeShard pins the shard's generation, opens the query's cursors,
// and runs the phase-1 traversal. OpNot terms wait for the exclusion
// cursor; this slice traverses the positive terms only.
func (cn *conn) executeShard(q *wire.Query, shard uint16, w *Worker) ([]Hit, error) {
	m, release, err := cn.s.reg.Acquire(shard)
	if err != nil {
		return nil, err
	}
	defer release()
	w.Reset()
	for _, t := range q.Terms {
		if t.Op == wire.OpNot {
			continue
		}
		if _, _, err := w.OpenTerm(m, t.Term); err != nil {
			return nil, err
		}
	}
	if w.open == 0 {
		return nil, nil
	}
	return w.Traverse(int(q.Candidates), nil)
}

// mergeHits folds one shard's hits into the cross-shard top-k, keeping
// entries score-descending. Docids go global as shard<<32 | local,
// enough for the root to route SNIP to the owning shard.
func mergeHits(entries []wire.ResultEntry, hits []Hit, shard uint16, k int) []wire.ResultEntry {
	for _, h := range hits {
		score := uint16(min(h.Score, math.MaxUint16))
		i := len(entries)
		for i > 0 && entries[i-1].Score < score {
			i--
		}
		if i >= k {
			continue
		}
		if len(entries) < k {
			entries = append(entries, wire.ResultEntry{})
		}
		copy(entries[i+1:], entries[i:])
		entries[i] = wire.ResultEntry{
			Docid: uint64(shard)<<32 | uint64(h.Docid),
			Score: score,
		}
	}
	return entries
}

// Dial is the client half used by the root and the replayer: it opens
// the connection and runs the HELLO exchange.
func Dial(addr string, nodeID uint64) (*Client, error) {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Client{c: nc, fr: wire.NewFrameReader(nc)}
	hello, err := wire.AppendHello(nil, &wire.Hello{
		Version: 1, NodeID: nodeID, MaxInflight: 1, MaxFrame: wire.MaxFrame,
	})
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	frame, err := wire.AppendFrame(nil, wire.KindHello, 0, hello)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	if _, err := nc.Write(frame); err != nil {
		_ = nc.Close()
		return nil, err
	}
	f, err := c.fr.Next()
	if err != nil || f.Kind != wire.KindHello {
		_ = nc.Close()
		return nil, fmt.Errorf("serve: handshake failed: %v", err)
	}
	if _, err := wire.ParseHello(f.Body); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

// Client is one synchronous connection to a serve node. One query in
// flight at a time; the replayer opens one client per concurrent lane.
type Client struct {
	c     net.Conn
	fr    *wire.FrameReader
	reqid uint64
}

// Query sends one QUERY and waits for its answer. A BUSY answer
// returns wire.ErrBusy semantics via the busy flag.
func (c *Client) Query(q *wire.Query) (*wire.QResult, bool, error) {
	c.reqid++
	body, err := wire.AppendQuery(nil, q)
	if err != nil {
		return nil, false, err
	}
	frame, err := wire.AppendFrame(nil, wire.KindQuery, c.reqid, body)
	if err != nil {
		return nil, false, err
	}
	if _, err := c.c.Write(frame); err != nil {
		return nil, false, err
	}
	for {
		f, err := c.fr.Next()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, false, err
		}
		if f.Reqid != c.reqid {
			continue // a late answer for a reqid this client abandoned
		}
		switch f.Kind {
		case wire.KindBusy:
			return nil, true, nil
		case wire.KindQResult:
			r, err := wire.ParseQResult(f.Body)
			return r, false, err
		default:
			return nil, false, fmt.Errorf("serve: unexpected frame kind %d", f.Kind)
		}
	}
}

// Ping round-trips a PING frame.
func (c *Client) Ping() error {
	c.reqid++
	frame, err := wire.AppendFrame(nil, wire.KindPing, c.reqid, nil)
	if err != nil {
		return err
	}
	if _, err := c.c.Write(frame); err != nil {
		return err
	}
	f, err := c.fr.Next()
	if err != nil {
		return err
	}
	if f.Kind != wire.KindPing || f.Reqid != c.reqid {
		return fmt.Errorf("serve: unexpected ping answer kind %d", f.Kind)
	}
	return nil
}

// Cancel fires a CANCEL for the last query's reqid.
func (c *Client) Cancel() error {
	frame, err := wire.AppendFrame(nil, wire.KindCancel, c.reqid, nil)
	if err != nil {
		return err
	}
	_, err = c.c.Write(frame)
	return err
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }
