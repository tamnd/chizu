package s3c

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

// The request counter backs the CG2 gate, so it must count what actually
// went over the wire: every attempt, retries included.
func TestRequestCounter(t *testing.T) {
	var calls atomic.Int32
	c, _ := fakeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte("x"))
	}))
	if c.Requests() != 0 {
		t.Fatalf("fresh client counts %d requests", c.Requests())
	}
	if _, _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := c.Requests(); got != 1 {
		t.Fatalf("one Get counted as %d", got)
	}
	// The second Get eats a 500 and retries, so it counts twice.
	if _, _, err := c.Get(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if got := c.Requests(); got != 3 {
		t.Fatalf("retried Get left the counter at %d, want 3", got)
	}
}
