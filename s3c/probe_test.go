package s3c

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// condStore is a one-bucket fake whose conditional-write honesty is
// configurable, so the probe's verdicts can be forced. dropPut, when set,
// is consulted per PUT with the body and a counter; returning drop kills
// the connection without a response (the MinIO EOF-after-412 shape), and
// applied says whether the write lands first.
type condStore struct {
	mu            sync.Mutex
	objects       map[string][]byte
	honorNoneNone bool // ignore If-None-Match: * entirely
	honorNoMatch  bool // ignore If-Match entirely
	puts          int
	dropPut       func(body []byte, n int) (drop, applied bool)
}

func (s *condStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/b/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		s.puts++
		var drop, applied bool
		if s.dropPut != nil {
			drop, applied = s.dropPut(body, s.puts)
		}
		if drop && !applied {
			hijackAndClose(w)
			return
		}
		_, exists := s.objects[key]
		if exists && r.Header.Get("If-None-Match") == "*" && !s.honorNoneNone {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if im := r.Header.Get("If-Match"); im != "" && !s.honorNoMatch {
			if !exists || im != `"`+condETag(s.objects[key])+`"` {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		s.objects[key] = body
		if drop {
			hijackAndClose(w)
			return
		}
		w.Header().Set("ETag", `"`+condETag(body)+`"`)
	case http.MethodGet:
		body, exists := s.objects[key]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+condETag(body)+`"`)
		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func hijackAndClose(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func condETag(body []byte) string {
	if len(body) == 0 {
		return "empty"
	}
	return string(body[:1]) // distinct per probe payload, enough for the fake
}

func TestProbeHonestBucket(t *testing.T) {
	c, _ := fakeClient(t, &condStore{objects: map[string][]byte{}})
	ifMatch, err := c.ProbeConditionalWrites(context.Background(), "probe/cas")
	if err != nil || !ifMatch {
		t.Fatalf("ifMatch %v err %v", ifMatch, err)
	}
}

func TestProbeIfMatchIgnored(t *testing.T) {
	c, _ := fakeClient(t, &condStore{objects: map[string][]byte{}, honorNoMatch: true})
	ifMatch, err := c.ProbeConditionalWrites(context.Background(), "probe/cas")
	if err != nil || ifMatch {
		t.Fatalf("ifMatch %v err %v; want the rootv fallback verdict", ifMatch, err)
	}
}

func TestProbeCreateExclusiveIgnoredIsFatal(t *testing.T) {
	c, _ := fakeClient(t, &condStore{objects: map[string][]byte{}, honorNoneNone: true})
	if _, err := c.ProbeConditionalWrites(context.Background(), "probe/cas"); err == nil {
		t.Fatal("a bucket ignoring If-None-Match must fail the probe")
	}
}

func TestProbeCleansUpStaleKey(t *testing.T) {
	st := &condStore{objects: map[string][]byte{"probe/cas": []byte("leftover")}}
	c, _ := fakeClient(t, st)
	ifMatch, err := c.ProbeConditionalWrites(context.Background(), "probe/cas")
	if err != nil || !ifMatch {
		t.Fatalf("ifMatch %v err %v", ifMatch, err)
	}
	st.mu.Lock()
	_, exists := st.objects["probe/cas"]
	st.mu.Unlock()
	if exists {
		t.Fatal("probe left its key behind")
	}
}

// TestProbeResolvesDroppedConnections is the MinIO flake: the bucket drops
// the connection on a conditional PUT without responding, so the client
// sees EOF and cannot know whether the write landed. The probe resolves
// that by reading the key back, whichever way the race went.
func TestProbeResolvesDroppedConnections(t *testing.T) {
	// One PUT of each payload dies before applying: the probe must retry
	// and still reach the honest verdict.
	dropped := map[string]bool{}
	st := &condStore{objects: map[string][]byte{}, dropPut: func(body []byte, n int) (bool, bool) {
		if !dropped[string(body)] {
			dropped[string(body)] = true
			return true, false
		}
		return false, false
	}}
	c, _ := fakeClient(t, st)
	ifMatch, err := c.ProbeConditionalWrites(context.Background(), "probe/cas")
	if err != nil || !ifMatch {
		t.Fatalf("ifMatch %v err %v", ifMatch, err)
	}
	if len(dropped) != 3 {
		t.Fatalf("dropped %d distinct payloads, want 3", len(dropped))
	}
}

func TestProbeResolvesLandedThenDropped(t *testing.T) {
	// The final replace lands and then the connection dies: the probe
	// must read the key back, see its payload, and call it a win.
	st := &condStore{objects: map[string][]byte{}, dropPut: func(body []byte, n int) (bool, bool) {
		return string(body) == "must win", true
	}}
	c, _ := fakeClient(t, st)
	ifMatch, err := c.ProbeConditionalWrites(context.Background(), "probe/cas")
	if err != nil || !ifMatch {
		t.Fatalf("ifMatch %v err %v", ifMatch, err)
	}
}

func TestLiveProbe(t *testing.T) {
	c, ctx := liveClient(t)
	ifMatch, err := c.ProbeConditionalWrites(ctx, testKey(t, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if !ifMatch {
		t.Fatal("MinIO honors If-Match; the probe must say so")
	}
	if _, _, err := c.Get(ctx, testKey(t, "cas")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("probe key survived: %v", err)
	}
}
