package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAlignedOffset(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const span = 1 << 20
	for range 10_000 {
		off := alignedOffset(rng, span, opBlk)
		if off%opBlk != 0 {
			t.Fatalf("offset %d not aligned", off)
		}
		if off < 0 || off+opBlk > span {
			t.Fatalf("offset %d out of range", off)
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
	path := filepath.Join(t.TempDir(), "mp.dat")
	f, err := ensureFile(path, 4<<20, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, _ := os.Stat(path)
	if st.Size() != 4<<20 {
		t.Fatalf("backing file %d bytes, want %d", st.Size(), 4<<20)
	}

	lat := preadLoop(f, 4<<20, 4, 0.05, 7)
	if len(lat) == 0 {
		t.Fatal("preadLoop: no samples")
	}

	m, err := syscall.Mmap(int(f.Fd()), 0, 4<<20, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Munmap(m) }()
	lat = mmapLoop(m, 4, 0.05, 7)
	if len(lat) == 0 {
		t.Fatal("mmapLoop: no samples")
	}
}

func TestAntagonistLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mp.dat")
	f, err := ensureFile(path, 4<<20, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	stop := startAntagonist("scan", path, 0)
	lat := preadLoop(f, 4<<20, 2, 0.05, 7)
	stop()
	if len(lat) == 0 {
		t.Fatal("preadLoop under scan: no samples")
	}

	stop = startAntagonist("none", path, 0)
	stop()
	stop = startAntagonist("hog", path, 0)
	stop()
}

func TestMajorFaults(t *testing.T) {
	if n := majorFaults(); n < 0 {
		t.Fatalf("majorFaults = %d, want >= 0", n)
	}
}
