// Lab mmap-vs-pread (doc 05 section 11): the read discipline decision
// for high-fan-out bands. The design says postings, skips, positions,
// and doc band go through pooled aligned pread, never mmap, because a
// page fault under memory pressure blocks an OS thread at an unplanned
// point while a pread miss blocks where the planner chose. This lab is
// the data that keeps or overturns that default.
//
// Both disciplines do the same work per op: 16 KiB from a random
// aligned offset of a cache-cold file lands in a caller buffer. pread
// uses ReadAt into a per-worker buffer; mmap copies the span out of a
// shared mapping, so a cold op pays the fault inside the copy.
//
// Configs are (discipline, workers, antagonist). Antagonists model the
// loaded serving box:
//
//   - none: quiet baseline.
//   - scan: a goroutine sequentially reads the whole file at full
//     speed through its own fd, thrashing the page cache and competing
//     for device bandwidth. This is the page-cache-thrash failure mode
//     doc 05 names.
//   - hog: anonymous memory sized to squeeze the page cache (default
//     12 GiB, -hog), touched once and held resident, so reclaim is
//     always breathing down the mapping's neck.
//
// Output is TSV: label arm workers antag samples p50us p99us p999us
// reads_s mbps majflt. majflt is the process major-fault delta for the
// config (Linux, 0 elsewhere): cold mmap rows should show roughly one
// major fault per op and pread rows roughly none, which pins the tail
// difference on the fault path rather than the device.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	opBlk     = 16 << 10 // planner read unit, matches lab 01's batch arms
	scanChunk = 4 << 20
)

func main() {
	label := flag.String("label", "local", "row label, name the host")
	path := flag.String("file", "mmapvspread.dat", "backing file on the disk under test")
	sizeGiB := flag.Int("size", 48, "backing file GiB; at least 2x RAM for honest cold rows")
	secs := flag.Float64("sec", 2.0, "seconds per config")
	seed := flag.Int64("seed", 2107, "rng seed")
	hogGiB := flag.Int("hog", 12, "anonymous memory GiB for the hog antagonist")
	antags := flag.String("antags", "none,scan,hog", "comma list")
	flag.Parse()

	size := int64(*sizeGiB) << 30
	f, err := ensureFile(*path, size, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmapvspread:", err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()

	m, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmapvspread: mmap:", err)
		os.Exit(1)
	}
	defer func() { _ = syscall.Munmap(m) }()

	fmt.Println("label\tarm\tworkers\tantag\tsamples\tp50us\tp99us\tp999us\treads_s\tmbps\tmajflt")
	for antag := range strings.SplitSeq(*antags, ",") {
		stop := startAntagonist(antag, *path, *hogGiB)
		for _, arm := range []string{"pread", "mmap"} {
			for _, workers := range []int{4, 16, 64} {
				dropCaches(*label)
				mf0 := majorFaults()
				var lat []float64
				if arm == "pread" {
					lat = preadLoop(f, size, workers, *secs, *seed)
				} else {
					lat = mmapLoop(m, workers, *secs, *seed)
				}
				report(*label, arm, workers, antag, lat, *secs, majorFaults()-mf0)
			}
		}
		stop()
	}
}

// ensureFile creates or extends the backing file with seeded random
// bytes so no filesystem can cheat with holes; an existing file of the
// right size is reused across runs (and shared with lab 01 if pointed
// at the same path).
func ensureFile(path string, size int64, seed int64) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() >= size {
		return os.Open(path)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	buf := make([]byte, 4<<20)
	for off := int64(0); off < size; off += int64(len(buf)) {
		rng.Read(buf)
		if _, err := f.WriteAt(buf, off); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return os.Open(path)
}

// preadLoop runs workers goroutines in a closed loop of random aligned
// preads for secs; returns per-op latencies in microseconds.
func preadLoop(f *os.File, span int64, workers int, secs float64, seed int64) []float64 {
	var mu sync.Mutex
	var all []float64
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewSource(seed + int64(w)))
			buf := make([]byte, opBlk)
			var lat []float64
			for time.Now().Before(deadline) {
				off := alignedOffset(rng, span, opBlk)
				t0 := time.Now()
				if _, err := f.ReadAt(buf, off); err != nil {
					fmt.Fprintln(os.Stderr, "mmapvspread: pread:", err)
					break
				}
				lat = append(lat, float64(time.Since(t0).Nanoseconds())/1e3)
			}
			mu.Lock()
			all = append(all, lat...)
			mu.Unlock()
		})
	}
	wg.Wait()
	return all
}

// mmapLoop is the same closed loop with the copy pulling from the
// mapping, so a cold span pays its page faults inside the timed copy.
func mmapLoop(m []byte, workers int, secs float64, seed int64) []float64 {
	span := int64(len(m))
	var mu sync.Mutex
	var all []float64
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewSource(seed + int64(w)))
			buf := make([]byte, opBlk)
			var lat []float64
			for time.Now().Before(deadline) {
				off := alignedOffset(rng, span, opBlk)
				t0 := time.Now()
				copy(buf, m[off:off+opBlk])
				lat = append(lat, float64(time.Since(t0).Nanoseconds())/1e3)
			}
			mu.Lock()
			all = append(all, lat...)
			mu.Unlock()
		})
	}
	wg.Wait()
	return all
}

// startAntagonist launches the named background load and returns a
// function that stops it and releases what it held.
func startAntagonist(name, path string, hogGiB int) func() {
	switch name {
	case "scan":
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			f, err := os.Open(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "mmapvspread: scan:", err)
				return
			}
			defer func() { _ = f.Close() }()
			st, _ := f.Stat()
			buf := make([]byte, scanChunk)
			off := int64(0)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := f.ReadAt(buf, off); err != nil {
					off = 0
					continue
				}
				off += scanChunk
				if off >= st.Size() {
					off = 0
				}
			}
		}()
		return func() { close(stop); <-done }
	case "hog":
		pages := hogGiB << 18 // 4 KiB pages per GiB
		hog := make([][]byte, 0, pages/1024)
		for range pages / 1024 {
			b := make([]byte, 4<<20)
			for i := 0; i < len(b); i += 4096 {
				b[i] = 1
			}
			hog = append(hog, b)
		}
		return func() {
			for i := range hog {
				hog[i] = nil
			}
			debug.FreeOSMemory()
		}
	default:
		return func() {}
	}
}

// alignedOffset draws a block-aligned offset with the whole block in
// range.
func alignedOffset(rng *rand.Rand, span int64, block int) int64 {
	blocks := span / int64(block)
	return rng.Int63n(blocks) * int64(block)
}

// majorFaults returns the process's cumulative major page faults from
// /proc/self/stat, or 0 where that file does not exist.
func majorFaults() int64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	// After the comm field: state ppid pgrp session tty tpgid flags
	// minflt cminflt majflt ...
	if len(fields) < 10 {
		return 0
	}
	n, err := strconv.ParseInt(fields[9], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// dropCaches asks the kernel for a cold page cache; on failure the row
// is marked so a warm-cache sweep can never pass silently as cold.
func dropCaches(label string) {
	_ = exec.Command("sync").Run()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
		fmt.Printf("# %s: drop_caches failed (%v); rows below are NOT cold\n", label, err)
	}
	time.Sleep(200 * time.Millisecond)
}

// report prints one TSV row.
func report(label, arm string, workers int, antag string, lat []float64, secs float64, majflt int64) {
	if len(lat) == 0 {
		fmt.Printf("# %s %s workers=%d antag=%s: no samples\n", label, arm, workers, antag)
		return
	}
	sort.Float64s(lat)
	fmt.Printf("%s\t%s\t%d\t%s\t%d\t%.1f\t%.1f\t%.1f\t%.0f\t%.0f\t%d\n",
		label, arm, workers, antag, len(lat),
		pct(lat, 50), pct(lat, 99), pct(lat, 99.9),
		float64(len(lat))/secs, float64(len(lat))*opBlk/secs/1e6, majflt)
}

// pct returns the q-th percentile of sorted samples, nearest-rank.
func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}
