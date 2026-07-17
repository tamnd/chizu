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
// configurable, so the probe's verdicts can be forced.
type condStore struct {
	mu            sync.Mutex
	objects       map[string][]byte
	honorNoneNone bool // ignore If-None-Match: * entirely
	honorNoMatch  bool // ignore If-Match entirely
}

func (s *condStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/b/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
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
		w.Header().Set("ETag", `"`+condETag(body)+`"`)
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
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
