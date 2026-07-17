package main

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestDevTokenize(t *testing.T) {
	got := devTokenize("The Andes: mountain-chain, 2nd try!")
	want := []string{"the", "andes", "mountain", "chain", "2nd", "try"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenize drifted: %v", got)
	}
	if toks := devTokenize(""); len(toks) != 0 {
		t.Fatalf("empty input gave %v", toks)
	}
}

// TestDevVerticalOffline runs the whole crawl-build-serve-root vertical
// without a bucket: corpus to shard bytes, shard through the serve stub,
// query and snippets over real frames on a loopback socket. This is the
// lane every CI job runs; the s3-suite lane adds the bucket hops.
func TestDevVerticalOffline(t *testing.T) {
	rows := devCorpus()
	shardBytes, err := devBuildShard(rows)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := openDevShard(shardBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	if sh.docCount != uint32(len(rows)) {
		t.Fatalf("shard holds %d docs for %d rows", sh.docCount, len(rows))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- devServe(ln, sh) }()

	res, snips, err := devRootRound(ln.Addr().String(), devQueryTerms, 3)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve stub did not shut down")
	}

	if err := devCheckAnswers(res, snips); err != nil {
		t.Fatal(err)
	}
	// The runners-up: amazon at 3 (two body rivers plus the title), fuji
	// at 2 (its title says mount, not mountain).
	if res.Entries[1].Docid != 3 || res.Entries[2].Docid != 0 {
		t.Fatalf("runner-up order drifted: %+v", res.Entries)
	}
}

// TestDevHarness runs `chizu dev` proper against the bucket, CG2
// assertion included; dev fails itself on any bucket request during
// the query round.
func TestDevHarness(t *testing.T) {
	if os.Getenv("CHIZU_S3_ENDPOINT") == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if os.Getenv("CHIZU_S3_BUCKET") == "" {
		t.Setenv("CHIZU_S3_BUCKET", "chizu-test")
	}
	prefix := fmt.Sprintf("test/dev-%d/", time.Now().UnixNano())
	if err := dev([]string{"-prefix", prefix}); err != nil {
		t.Fatal(err)
	}
	// A second run reuses the existing root instead of refusing.
	if err := dev([]string{"-prefix", prefix}); err != nil {
		t.Fatal(err)
	}
}

func TestDevRequiresBucketEnv(t *testing.T) {
	if os.Getenv("CHIZU_S3_ENDPOINT") != "" {
		t.Skip("bucket env is set; the offline lanes cover the refusal")
	}
	if err := dev(nil); err == nil {
		t.Fatal("dev without CHIZU_S3_ENDPOINT must refuse")
	}
}
