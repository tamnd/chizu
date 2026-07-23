package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/tamnd/chizu/s3c"
)

// TestUploadHotConsume proves the consuming upload sends the exact
// bytes while punching the source behind acknowledged parts: the
// remote object hashes identical to the pre-upload file, and on Linux
// the local file ends the upload holding almost no blocks.
func TestUploadHotConsume(t *testing.T) {
	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "chizu-test"
	}
	client, err := s3c.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := client.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}

	// 12 MB at the 5 MB minimum part size: two full parts and a tail.
	data := make([]byte, 12<<20)
	rand.New(rand.NewSource(2107)).Read(data)
	want := sha256.Sum256(data)
	path := filepath.Join(t.TempDir(), "shard.hot")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	key := fmt.Sprintf("test/uploadconsume-%d/shard.hot", time.Now().UnixNano())
	if err := UploadHotConsume(ctx, client, key, path, minPartSize); err != nil {
		t.Fatal(err)
	}

	got, _, err := client.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("remote bytes differ from the pre-upload file")
	}
	if !bytes.Equal(got[:64], data[:64]) {
		t.Fatal("remote head bytes differ")
	}

	if runtime.GOOS == "linux" {
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		if st.Blocks*512 > 1<<20 {
			t.Fatalf("consumed source still holds %d bytes of blocks", st.Blocks*512)
		}
	}
}
