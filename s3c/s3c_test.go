package s3c

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// liveClient returns a client against the environment's bucket, or skips.
// The s3-suite CI lane and local podman/docker MinIO both provide the
// CHIZU_S3_* variables; the hermetic CI legs skip these tests.
func liveClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	cfg := FromEnv()
	if cfg.Endpoint == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "chizu-test"
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return c, ctx
}

func testKey(t *testing.T, suffix string) string {
	return "test/" + strings.ToLower(t.Name()) + "/" + suffix
}

func TestLiveRoundTrip(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "obj")
	putETag, err := c.Put(ctx, key, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	data, getETag, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" || getETag != putETag {
		t.Fatalf("got %q etag %q (put etag %q)", data, getETag, putETag)
	}
	info, err := c.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 5 || info.ETag != putETag {
		t.Fatalf("head: %+v", info)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	// Deleting a missing key succeeds, per S3.
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
}

func TestLiveRange(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "obj")
	blob := make([]byte, 1024)
	for i := range blob {
		blob[i] = byte(i*31 + 7)
	}
	if _, err := c.Put(ctx, key, blob); err != nil {
		t.Fatal(err)
	}
	defer c.Delete(ctx, key)
	got, err := c.GetRange(ctx, key, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob[10:26]) {
		t.Fatalf("range mismatch: %v vs %v", got, blob[10:26])
	}
}

func TestLiveCreateExclusive(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "obj")
	defer c.Delete(ctx, key)
	if _, err := c.CreateExclusive(ctx, key, []byte("winner")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := c.CreateExclusive(ctx, key, []byte("loser"))
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second create: want ErrPrecondition, got %v", err)
	}
	data, _, err := c.Get(ctx, key)
	if err != nil || string(data) != "winner" {
		t.Fatalf("winner not preserved: %q %v", data, err)
	}
}

func TestLiveReplaceIfMatch(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "obj")
	defer c.Delete(ctx, key)
	etag1, err := c.Put(ctx, key, []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	etag2, err := c.ReplaceIfMatch(ctx, key, []byte("v2"), etag1)
	if err != nil {
		t.Fatalf("replace with current etag: %v", err)
	}
	if etag2 == etag1 {
		t.Fatal("etag did not change")
	}
	if _, err := c.ReplaceIfMatch(ctx, key, []byte("v3"), etag1); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale replace: want ErrPrecondition, got %v", err)
	}
	data, _, err := c.Get(ctx, key)
	if err != nil || string(data) != "v2" {
		t.Fatalf("state after stale replace: %q %v", data, err)
	}
}

func TestLiveDeleteBatch(t *testing.T) {
	c, ctx := liveClient(t)
	var keys []string
	for i := range 5 {
		k := testKey(t, fmt.Sprintf("obj%d", i))
		if _, err := c.Put(ctx, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	keys = append(keys, testKey(t, "never-existed"))
	if err := c.DeleteBatch(ctx, keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if _, _, err := c.Get(ctx, k); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s still there: %v", k, err)
		}
	}
}

func patternBytes(size int64) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(int64(i)*31 + 7)
	}
	return b
}

func TestLiveMultipartUpload(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "big")
	defer c.Delete(ctx, key)

	const size = 11 << 20 // three parts at 5 MiB: 5+5+1
	want := patternBytes(size)
	etag, err := c.Upload(ctx, key, bytes.NewReader(want), 5<<20, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if etag == "" {
		t.Fatal("empty etag from complete")
	}
	info, err := c.Head(ctx, key)
	if err != nil || info.Size != size {
		t.Fatalf("head after upload: %+v %v", info, err)
	}
	// Read across the first part boundary.
	got, err := c.GetRange(ctx, key, 5<<20-8, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[5<<20-8:5<<20+8]) {
		t.Fatal("bytes differ across the part boundary")
	}
	full, _, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, want) {
		t.Fatal("full object differs from uploaded bytes")
	}
}

func TestLiveMultipartExclusive(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "big")
	defer c.Delete(ctx, key)

	blob := patternBytes(6 << 20)
	if _, err := c.Upload(ctx, key, bytes.NewReader(blob), 5<<20, 2, true); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 501 {
			t.Skipf("server does not implement conditional multipart complete: %v", err)
		}
		t.Fatal(err)
	}
	_, err := c.Upload(ctx, key, bytes.NewReader(blob), 5<<20, 2, true)
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second exclusive upload: want ErrPrecondition, got %v", err)
	}
	data, _, err := c.Get(ctx, key)
	if err != nil || !bytes.Equal(data, blob) {
		t.Fatalf("winner not intact after losing upload: %v", err)
	}
}

func TestLiveAbortLeavesNothing(t *testing.T) {
	c, ctx := liveClient(t)
	key := testKey(t, "aborted")
	uploadID, err := c.MultipartCreate(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.MultipartPut(ctx, key, uploadID, 1, patternBytes(5<<20)); err != nil {
		t.Fatal(err)
	}
	if err := c.MultipartAbort(ctx, key, uploadID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aborted upload left a visible object: %v", err)
	}
}
