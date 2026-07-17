package s3c

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func fakeClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c, err := New(Config{
		Endpoint:    ts.URL,
		Bucket:      "b",
		AccessKey:   "k",
		SecretKey:   "s",
		PathStyle:   true,
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, ts
}

func TestRetryOn5xxThenSucceed(t *testing.T) {
	var calls atomic.Int32
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.Write([]byte("payload"))
	}))
	data, etag, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" || etag != "abc" {
		t.Fatalf("got %q etag %q", data, etag)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls.Load())
	}
}

func TestWriteRetriesOn5xxToo(t *testing.T) {
	// A 5xx is a definite rejection, so even a conditional write replays.
	var calls atomic.Int32
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"e2"`)
	}))
	etag, err := c.CreateExclusive(context.Background(), "k", []byte("v"))
	if err != nil || etag != "e2" {
		t.Fatalf("etag %q err %v", etag, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestPreconditionIsTerminal(t *testing.T) {
	var calls atomic.Int32
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	_, err := c.ReplaceIfMatch(context.Background(), "k", []byte("v"), "stale")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("want ErrPrecondition, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("412 must not retry; got %d attempts", calls.Load())
	}
}

func TestNotFound(t *testing.T) {
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, _, err := c.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestWriteTransportFailureIsAmbiguous(t *testing.T) {
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}))
	_, err := c.Put(context.Background(), "k", []byte("v"))
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("want AmbiguousError, got %v", err)
	}
}

func TestGiveUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_, _, err := c.Get(context.Background(), "k")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusInternalServerError {
		t.Fatalf("want wrapped APIError 500, got %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected MaxAttempts=3 attempts, got %d", calls.Load())
	}
}
