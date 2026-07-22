package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGovMilestones(t *testing.T) {
	start := time.Now()
	g := newGov(start, 10*time.Millisecond)
	if got := g.t1.Sub(start); got != 6*time.Millisecond {
		t.Fatalf("t1 = %v, want 6ms", got)
	}
	if got := g.t2.Sub(start); got != 7500*time.Microsecond {
		t.Fatalf("t2 = %v, want 7.5ms", got)
	}
	if got := g.t3.Sub(start); got != 9500*time.Microsecond {
		t.Fatalf("t3 = %v, want 9.5ms", got)
	}
	if g.bits != 0 {
		t.Fatalf("fresh governor carries bits %b", g.bits)
	}
}

func TestClassDraw(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, cl := range classes {
		var sample []float64
		for range 20_000 {
			n := cl.draw(rng)
			if n < 1 {
				t.Fatalf("%s drew %d blocks", cl.name, n)
			}
			sample = append(sample, float64(n))
		}
		// The lognormal median must land near the fitted p50.
		med := median(sample)
		if med < cl.p50*0.7 || med > cl.p50*1.4 {
			t.Fatalf("%s median %f far from fitted p50 %f", cl.name, med, cl.p50)
		}
	}
}

func median(xs []float64) float64 {
	s := append([]float64{}, xs...)
	sortFloats(s)
	return s[len(s)/2]
}

func sortFloats(s []float64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestPickMixCoversClasses(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	seen := map[string]int{}
	for range 10_000 {
		seen[pickMix(rng).name]++
	}
	for _, c := range classes {
		if seen[c.name] == 0 {
			t.Fatalf("mix never drew class %s", c.name)
		}
	}
	// stop is the rarest at 5%; it must stay well under torso's 40%.
	if seen["stop"] >= seen["torso"] {
		t.Fatalf("mix weights broken: stop %d >= torso %d", seen["stop"], seen["torso"])
	}
}

func TestRunQuerySmoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "back.dat")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := &worker{f: f, size: 1 << 20, rng: rand.New(rand.NewSource(3))}
	for i := range w.bufs {
		w.bufs[i] = make([]byte, blockRead)
	}
	for _, cl := range classes {
		lat, bits := w.runQuery(cl, 10*time.Millisecond)
		if lat <= 0 {
			t.Fatalf("%s: non-positive latency", cl.name)
		}
		if bits&^(dTerms|dK|dBlocks) != 0 {
			t.Fatalf("%s: unknown degrade bits %b", cl.name, bits)
		}
	}
}

func TestPct(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := pct(sorted, 50); got != 5 {
		t.Fatalf("p50 = %v, want 5", got)
	}
	if got := pct(sorted, 100); got != 10 {
		t.Fatalf("p100 = %v, want 10", got)
	}
	if got := pct(nil, 50); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
	if math.IsNaN(pct(sorted, 99.9)) {
		t.Fatal("NaN percentile")
	}
}
