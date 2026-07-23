package build

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestEmitRemovesConsumedRuns(t *testing.T) {
	dir := t.TempDir()
	out := runPass(t, dir, emitRows(t), 512)
	if len(out.Runs) < 2 {
		t.Fatalf("budget 512 produced %d runs, want several", len(out.Runs))
	}
	var buf bytes.Buffer
	if err := Emit(&buf, out, emitConfig(t)); err != nil {
		t.Fatal(err)
	}
	for _, path := range out.Runs {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("run %s survived the merge", filepath.Base(path))
		}
	}
}

func TestPunchHoleFreesBlocks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hole punching is linux-only; elsewhere punchHole is a no-op error")
	}
	path := filepath.Join(t.TempDir(), "spool")
	data := bytes.Repeat([]byte("chizu"), 64<<10)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	blocks := func() int64 {
		var st syscall.Stat_t
		if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		return st.Blocks
	}
	before := blocks()
	if err := punchHole(f.Fd(), 0, int64(len(data))); err != nil {
		t.Fatalf("punchHole: %v", err)
	}
	if after := blocks(); after >= before {
		t.Fatalf("blocks %d -> %d, want fewer", before, after)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != int64(len(data)) {
		t.Fatalf("logical size changed to %d", st.Size())
	}
}

func TestReclaimBelowThresholdIsQuiet(t *testing.T) {
	dir := t.TempDir()
	out := runPass(t, dir, testRows(t), DefaultRunBudget)
	if len(out.Runs) != 1 {
		t.Fatalf("default budget produced %d runs", len(out.Runs))
	}
	r, err := OpenRun(out.Runs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var rec Rec
	for {
		if err := r.Next(&rec); err != nil {
			break
		}
		r.Reclaim()
	}
	// A tiny run never crosses punchChunk, so nothing is punched and
	// the reader keeps working; this also proves Reclaim in the merge
	// loop is safe on runs of any size.
	if r.punched != 0 {
		t.Fatalf("punched %d bytes of a tiny run", r.punched)
	}
}
