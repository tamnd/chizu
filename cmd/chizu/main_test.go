package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/s3c"
)

func TestVersionIsSet(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty")
	}
}

func TestAdminCreate(t *testing.T) {
	if os.Getenv("CHIZU_S3_ENDPOINT") == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if os.Getenv("CHIZU_S3_BUCKET") == "" {
		t.Setenv("CHIZU_S3_BUCKET", "chizu-test")
	}
	prefix := fmt.Sprintf("test/admincreate-%d/", time.Now().UnixNano())

	if err := adminCreate([]string{"-prefix", prefix, "-p", "512", "-shard-size", "100000"}); err != nil {
		t.Fatal(err)
	}

	client, err := s3c.New(s3c.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	rs := chain.NewRootStore(client, prefix, 1, false)
	root, err := rs.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if root.P != 512 || root.ShardSize != 100000 || root.CkptSeq != 0 {
		t.Fatalf("root: %+v", root)
	}
	if string(root.Frozen) != "law=1 tok=1 quant=1" {
		t.Fatalf("frozen: %q", root.Frozen)
	}
	if root.DBID == 0 {
		t.Fatal("dbid was not randomized")
	}

	// The probe cleans up after itself.
	if _, _, err := client.Get(t.Context(), prefix+"probe/cas"); err == nil {
		t.Fatal("probe key survived")
	}

	// A second create at the same prefix refuses.
	err = adminCreate([]string{"-prefix", prefix})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second create: %v", err)
	}
}

func TestAdminCreateRejectsBadKnobs(t *testing.T) {
	if err := adminCreate([]string{"-p", "70000"}); err == nil || !strings.Contains(err.Error(), "u16") {
		t.Fatalf("p out of range: %v", err)
	}
	if err := adminCreate([]string{"-shard-size", "0"}); err == nil || !strings.Contains(err.Error(), "u32") {
		t.Fatalf("shard-size zero: %v", err)
	}
}
