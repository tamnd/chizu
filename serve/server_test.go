package serve

import (
	"net"
	"testing"
	"time"

	"github.com/tamnd/chizu/wire"
)

// startServer publishes the traversal fixture shard (shard number 7)
// and serves it on a loopback listener.
func startServer(t *testing.T, terms []travTerm, opts ServerOptions) (*Server, string) {
	t.Helper()
	m := travMount(t, terms)
	reg := NewRegistry()
	if err := reg.Publish(m); err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, opts)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Close() })
	return s, l.Addr().String()
}

func testQuery(names []string, k, candidates uint16) *wire.Query {
	q := &wire.Query{
		BudgetUS:   1_000_000,
		K:          k,
		Candidates: candidates,
		Shards:     []uint16{7},
	}
	for _, n := range names {
		q.Terms = append(q.Terms, wire.QueryTerm{Term: []byte(n)})
	}
	return q
}

func TestServerQuery(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{NodeID: 42})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}

	qn := []string{"t00", "t04", "t09"}
	gold := brute(terms, qn, 10, nil)
	r, busy, err := c.Query(testQuery(qn, 10, 100))
	if err != nil || busy {
		t.Fatalf("query: busy=%v err=%v", busy, err)
	}
	if r.Degrade != 0 {
		t.Fatalf("degrade bits %b on an easy query", r.Degrade)
	}
	if len(r.Entries) != len(gold) {
		t.Fatalf("%d entries, want %d", len(r.Entries), len(gold))
	}
	for i, e := range r.Entries {
		if int32(e.Score) != gold[i].Score {
			t.Fatalf("entry %d score %d, want %d", i, e.Score, gold[i].Score)
		}
		if e.Docid>>32 != 7 {
			t.Fatalf("entry %d docid %#x lost its shard tag", i, e.Docid)
		}
	}

	// The same connection answers again: the loop is stateless per query.
	r2, busy, err := c.Query(testQuery([]string{"t01"}, 5, 50))
	if err != nil || busy {
		t.Fatalf("second query: busy=%v err=%v", busy, err)
	}
	if len(r2.Entries) != 5 {
		t.Fatalf("second query returned %d entries", len(r2.Entries))
	}
}

func TestServerAbsentTerms(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r, busy, err := c.Query(testQuery([]string{"nosuchterm"}, 10, 100))
	if err != nil || busy {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
	if len(r.Entries) != 0 || r.Degrade != 0 {
		t.Fatalf("absent term produced %d entries, degrade %b", len(r.Entries), r.Degrade)
	}
}

func TestServerMissingShard(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	q := testQuery([]string{"t00"}, 10, 100)
	q.Shards = []uint16{99}
	r, busy, err := c.Query(q)
	if err != nil || busy {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
	if r.Degrade&DegradeShard == 0 {
		t.Fatalf("unmounted shard did not set DegradeShard: %b", r.Degrade)
	}
}

func TestServerExpiredBudget(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	q := testQuery([]string{"t00"}, 10, 100)
	q.BudgetUS = 1 // T3 is already behind us at receipt
	r, busy, err := c.Query(q)
	if err != nil || busy {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
	if r.Degrade&DegradeShard == 0 || len(r.Entries) != 0 {
		t.Fatalf("expired budget answered entries=%d degrade=%b", len(r.Entries), r.Degrade)
	}
}

func TestServerBusy(t *testing.T) {
	terms := genTravTerms(2107)
	s, addr := startServer(t, terms, ServerOptions{MaxInflight: 2})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Fill the in-flight slots by hand: the accounting, not the load,
	// is what the test pins.
	s.inflight.Add(2)
	_, busy, err := c.Query(testQuery([]string{"t00"}, 10, 100))
	if err != nil {
		t.Fatal(err)
	}
	if !busy {
		t.Fatal("over-cap query was not refused")
	}
	s.inflight.Add(-2)
	r, busy, err := c.Query(testQuery([]string{"t00"}, 10, 100))
	if err != nil || busy {
		t.Fatalf("post-drain query: busy=%v err=%v", busy, err)
	}
	if len(r.Entries) == 0 {
		t.Fatal("post-drain query returned nothing")
	}
}

func TestServerMalformedQueryClosesConn(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	frame, err := wire.AppendFrame(nil, wire.KindQuery, 9, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.c.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := c.fr.Next(); err == nil {
		t.Fatal("malformed query did not close the connection")
	}
}

func TestCancelBookkeeping(t *testing.T) {
	cn := &conn{
		cancelled: map[uint64]struct{}{},
		live:      map[uint64]struct{}{},
	}
	cn.begin(5)
	cn.cancel(5)
	if !cn.finish(5) {
		t.Fatal("cancel before finish was lost")
	}
	if cn.finish(5) {
		t.Fatal("finish is not idempotent on the cancel set")
	}
	cn.cancel(6) // never began: must drop silently
	cn.begin(7)
	if cn.finish(7) {
		t.Fatal("uncancelled query read as cancelled")
	}
}

func TestServerCancelUnknownReqid(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	frame, err := wire.AppendFrame(nil, wire.KindCancel, 12345, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.c.Write(frame); err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(); err != nil {
		t.Fatalf("connection died on a stray cancel: %v", err)
	}
}

func TestServerConcurrentQueries(t *testing.T) {
	terms := genTravTerms(2107)
	_, addr := startServer(t, terms, ServerOptions{MaxInflight: 8})
	qn := []string{"t00", "t01", "t02"}
	gold := brute(terms, qn, 10, nil)
	done := make(chan error, 8)
	for range 8 {
		go func() {
			c, err := Dial(addr, 1)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = c.Close() }()
			for range 20 {
				r, busy, err := c.Query(testQuery(qn, 10, 100))
				if err != nil {
					done <- err
					return
				}
				if busy {
					continue
				}
				if !sameScores(gold, toHits(r.Entries)) {
					t.Error("concurrent query scores diverged")
					done <- nil
					return
				}
			}
			done <- nil
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func toHits(entries []wire.ResultEntry) []Hit {
	hits := make([]Hit, len(entries))
	for i, e := range entries {
		hits[i] = Hit{Docid: uint32(e.Docid), Score: int32(e.Score)}
	}
	return hits
}

func TestServerCloseUnblocks(t *testing.T) {
	terms := genTravTerms(2107)
	s, addr := startServer(t, terms, ServerOptions{})
	c, err := Dial(addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	_ = c.c.SetReadDeadline(deadline)
	if _, err := c.fr.Next(); err == nil {
		t.Fatal("connection survived server close")
	}
}
