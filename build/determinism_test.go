package build

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tamnd/chizu/fixture"
)

// B2 provisional harness, doc 10 section 3: two builds of the fixture
// shard in different processes must be bit-identical (H-I1/B-I1). A
// fresh process gets fresh map iteration order, fresh address layout,
// and fresh GC timing, which is exactly the nondeterminism a same-
// process double build can hide. This is the standing suite until the
// build vertical extends it to whole nodes.

const (
	b2ChildEnv = "CHIZU_B2_BUILD_OUT"
	b2Seed     = 2107
	b2Docs     = 2000
	// A small budget forces several runs, so the merge is real.
	b2Budget = 1 << 20
)

// TestB2Child is the child half: invisible in normal runs, it builds
// the fixture shard and writes the .hot where the parent said to.
func TestB2Child(t *testing.T) {
	out := os.Getenv(b2ChildEnv)
	if out == "" {
		t.Skip("child half of the B2 harness; the parent test sets " + b2ChildEnv)
	}
	if err := buildFixtureShard(out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func buildFixtureShard(out, scratch string) error {
	c := fixture.New(b2Seed, b2Docs)
	p := NewShardPass(scratch, b2Budget)
	for docid := range uint64(b2Docs) {
		page := c.Page(docid)
		row := fixture.ToRow(&page)
		if err := p.AddRow(&row); err != nil {
			return err
		}
	}
	so, err := p.Finish()
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	cfg := &EmitConfig{
		SpoolDir:   scratch,
		Shard:      0,
		Generation: 1,
		Writer:     1,
		Builder:    1,
		BuildMS:    1_750_000_000_000,
		Watermarks: []uint64{1},
	}
	if err := Emit(f, so, cfg); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func TestBuildDeterminismAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns two child builds")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var sums [2][32]byte
	for i := range sums {
		out := filepath.Join(dir, string(rune('a'+i))+".hot")
		cmd := exec.Command(exe, "-test.run=^TestB2Child$", "-test.count=1")
		cmd.Env = append(os.Environ(), b2ChildEnv+"="+out)
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child build %d: %v\n%s", i, err, msg)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("child build %d wrote an empty shard", i)
		}
		sums[i] = sha256.Sum256(data)
	}
	if !bytes.Equal(sums[0][:], sums[1][:]) {
		t.Fatalf("fixture shard differs across processes: %x vs %x", sums[0], sums[1])
	}
}
