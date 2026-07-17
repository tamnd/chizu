package chain

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tamnd/chizu/s3c"
)

// store is a tiny in-memory bucket speaking just enough S3 for the append
// loop: GET, and PUT with If-None-Match: *. killNextPut simulates a timeout
// after the server received the write: the connection dies with no response,
// and storeOnKill decides whether the write landed first.
type store struct {
	mu          sync.Mutex
	objects     map[string][]byte
	puts        atomic.Int32
	killNextPut bool
	storeOnKill bool
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/b/")
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		body, ok := s.objects[key]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", fakeETag(body))
		_, _ = w.Write(body)
	case http.MethodPut:
		s.puts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		cur, exists := s.objects[key]
		if exists && r.Header.Get("If-None-Match") == "*" {
			s.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if im := r.Header.Get("If-Match"); im != "" && (!exists || im != fakeETag(cur)) {
			s.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if s.killNextPut {
			s.killNextPut = false
			if s.storeOnKill {
				s.objects[key] = body
			}
			s.mu.Unlock()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		s.objects[key] = body
		s.mu.Unlock()
		w.Header().Set("ETag", fakeETag(body))
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func fakeETag(body []byte) string {
	return fmt.Sprintf("%q", fmt.Sprintf("%08x", crc(body)))
}

func (s *store) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func fakeBucket(t *testing.T) (*s3c.Client, *store) {
	t.Helper()
	st := &store{objects: map[string][]byte{}}
	ts := httptest.NewServer(st)
	t.Cleanup(ts.Close)
	c, err := s3c.New(s3c.Config{
		Endpoint: ts.URL, Bucket: "b",
		AccessKey: "k", SecretKey: "s",
		PathStyle: true, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, st
}

type collected struct {
	seq    uint64
	writer uint64
	batch  uint64
}

func collector(got *[]collected) func(uint64, *Batch) {
	return func(seq uint64, b *Batch) {
		*got = append(*got, collected{seq: seq, writer: b.Writer, batch: b.BatchID})
	}
}

func TestAppendThenCatchUp(t *testing.T) {
	c, _ := fakeBucket(t)
	ctx := context.Background()
	w, err := Open(ctx, c, Options{Prefix: "db/", Writer: 7, Incarnation: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		seq, err := w.Append(ctx, []Record{&Ckpt{Seq: uint64(i)}})
		if err != nil {
			t.Fatal(err)
		}
		if seq != uint64(i) {
			t.Fatalf("append %d landed at %d", i, seq)
		}
	}

	var got []collected
	r, err := Open(ctx, c, Options{Prefix: "db/", Writer: 8, Incarnation: 1, Observe: collector(&got)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Pos() != 3 || len(got) != 3 {
		t.Fatalf("catch-up saw %d batches, pos %d", len(got), r.Pos())
	}
	for i, g := range got {
		if g.seq != uint64(i) || g.writer != 7 || g.batch != uint64(i+1) {
			t.Fatalf("batch %d: %+v", i, g)
		}
	}
}

func TestLostRaceAdvancesForFree(t *testing.T) {
	c, _ := fakeBucket(t)
	ctx := context.Background()
	var got []collected
	a, err := Open(ctx, c, Options{Prefix: "db/", Writer: 7, Incarnation: 1, Observe: collector(&got)})
	if err != nil {
		t.Fatal(err)
	}
	// A second writer wins slot 0 behind a's back.
	b, err := Open(ctx, c, Options{Prefix: "db/", Writer: 9, Incarnation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Append(ctx, []Record{&Ckpt{Seq: 99}}); err != nil {
		t.Fatal(err)
	}

	seq, err := a.Append(ctx, []Record{&Ckpt{Seq: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("loser should land at slot 1, got %d", seq)
	}
	// Observe saw the winner's slot 0 and then a's own landed batch at 1.
	if len(got) != 2 || got[0].writer != 9 || got[0].seq != 0 || got[1].writer != 7 || got[1].seq != 1 {
		t.Fatalf("loser did not observe winner then self: %+v", got)
	}
}

func TestAmbiguousOutcomeLanded(t *testing.T) {
	c, st := fakeBucket(t)
	ctx := context.Background()
	w, err := Open(ctx, c, Options{Prefix: "db/", Writer: 7, Incarnation: 1})
	if err != nil {
		t.Fatal(err)
	}
	st.killNextPut = true
	st.storeOnKill = true
	seq, err := w.Append(ctx, []Record{&Ckpt{Seq: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("landed append resolved to %d", seq)
	}
	if st.count() != 1 || st.puts.Load() != 1 {
		t.Fatalf("blind retry happened: %d objects, %d puts", st.count(), st.puts.Load())
	}
	if w.Pos() != 1 {
		t.Fatalf("pos %d after resolved append", w.Pos())
	}
}

func TestAmbiguousOutcomeNotLanded(t *testing.T) {
	c, st := fakeBucket(t)
	ctx := context.Background()
	w, err := Open(ctx, c, Options{Prefix: "db/", Writer: 7, Incarnation: 1})
	if err != nil {
		t.Fatal(err)
	}
	st.killNextPut = true
	st.storeOnKill = false
	seq, err := w.Append(ctx, []Record{&Ckpt{Seq: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("retried append landed at %d", seq)
	}
	// Two PUTs: the killed one and the replay after the slot proved empty.
	if st.count() != 1 || st.puts.Load() != 2 {
		t.Fatalf("%d objects, %d puts", st.count(), st.puts.Load())
	}
}

func TestPollSeesNewBatches(t *testing.T) {
	c, _ := fakeBucket(t)
	ctx := context.Background()
	w, err := Open(ctx, c, Options{Prefix: "db/", Writer: 7, Incarnation: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []collected
	f, err := Open(ctx, c, Options{Prefix: "db/", Writer: 8, Incarnation: 1, Observe: collector(&got)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := w.Append(ctx, []Record{&Ckpt{Seq: uint64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || f.Pos() != 2 {
		t.Fatalf("poll saw %d batches, pos %d", len(got), f.Pos())
	}
}

func TestCorruptSlotIsAnError(t *testing.T) {
	c, st := fakeBucket(t)
	ctx := context.Background()
	st.mu.Lock()
	st.objects["db/chain/0000000000000000"] = []byte("not a batch")
	st.mu.Unlock()
	if _, err := Open(ctx, c, Options{Prefix: "db/", Writer: 7, Incarnation: 1}); err == nil {
		t.Fatal("open decoded a corrupt slot without error")
	}
}
