package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tamnd/chizu/fixture"
)

// TestDevFixtureOffline runs the fixture vertical without a bucket:
// cold segments in, .hot file out, file-backed reopen, band
// verification, and the probe-term postings walk. Small segments force
// the corpus through several coldfmt round trips, and 1200 docs push
// the head terms well past the inline cutoff into multi-block FOR
// postings.
func TestDevFixtureOffline(t *testing.T) {
	const n = 1200
	dir := t.TempDir()
	c := fixture.New(devFixtureSeed, n)
	hotPath, err := devFixtureBuild(dir, c, 400, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := devFixtureCheck(hotPath, c, n); err != nil {
		t.Fatal(err)
	}
}

func TestDevFixtureProbes(t *testing.T) {
	c := fixture.New(devFixtureSeed, 100)
	probes := devFixtureProbes(c, 100)
	if len(probes) < 4 {
		t.Fatalf("only %d probe terms", len(probes))
	}
	for i, p := range probes {
		for _, q := range probes[i+1:] {
			if string(p) == string(q) {
				t.Fatalf("duplicate probe %q", p)
			}
		}
	}
}

// TestDevFixtureHarness runs `chizu dev -fixture` proper against the
// bucket, multipart upload included.
func TestDevFixtureHarness(t *testing.T) {
	if os.Getenv("CHIZU_S3_ENDPOINT") == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if os.Getenv("CHIZU_S3_BUCKET") == "" {
		t.Setenv("CHIZU_S3_BUCKET", "chizu-test")
	}
	prefix := fmt.Sprintf("test/devfix-%d/", time.Now().UnixNano())
	if err := dev([]string{"-prefix", prefix, "-fixture", "1000"}); err != nil {
		t.Fatal(err)
	}
}
