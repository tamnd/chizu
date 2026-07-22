package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestAlignedOffset(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const span = 1 << 20
	for _, block := range []int{4 << 10, 16 << 10, 64 << 10} {
		for range 10_000 {
			off := alignedOffset(rng, span, block)
			if off%int64(block) != 0 {
				t.Fatalf("block %d: offset %d not aligned", block, off)
			}
			if off < 0 || off+int64(block) > span {
				t.Fatalf("block %d: offset %d out of range", block, off)
			}
		}
	}
}

func TestPct(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := pct(sorted, 50); got != 5 {
		t.Fatalf("p50 = %v, want 5", got)
	}
	if got := pct(sorted, 99.9); got != 10 {
		t.Fatalf("p99.9 = %v, want 10", got)
	}
	if got := pct(nil, 50); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
}

func TestLoopsSmoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rp.dat")
	f, err := ensureFile(path, 4<<20, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := os.Stat(path)
	if st.Size() != 4<<20 {
		t.Fatalf("backing file %d bytes, want %d", st.Size(), 4<<20)
	}

	lat, reads := opLoop(f, 4<<20, 4<<10, 4, 0.05, 7)
	if len(lat) == 0 || reads != len(lat) {
		t.Fatalf("opLoop: %d samples, %d reads", len(lat), reads)
	}

	lat, reads = batchLoop(f, 4<<20, 8, 4, 4, 0.05, 7)
	if len(lat) == 0 {
		t.Fatal("batchLoop: no samples")
	}
	if reads%12 != 0 {
		t.Fatalf("batchLoop: %d reads not a multiple of batch size 12", reads)
	}
}
