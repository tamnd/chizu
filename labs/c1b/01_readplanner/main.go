// Lab read-planner (doc 07 sections 3 and 13): goroutine-pread against
// a cache-cold file on the gate box, versus block size, queue depth,
// and batch shape. The io_uring question (doc 01 section 10) closes on
// this lab's overhead rows: Go reaches NVMe depth with N goroutines in
// concurrent pread, and io_uring stays a road not taken unless the
// measured per-op overhead is the proven culprit in a Q1 miss.
//
// Arms:
//
//   - hot: preads from a page-cache-resident window, so the device is
//     out of the picture and the row is pure syscall plus scheduler
//     cost per op at depth. This is the microsecond-class claim.
//   - depth: cache-cold random preads, block sizes 4/16/64 KiB, depths
//     1..64; per-op latency percentiles, IOPS, MB/s. The device
//     envelope the planner budgets against.
//   - batch: the planner contract shape: a query needs B scattered
//     blocks, issued as one concurrent batch at depth D; batch
//     completion percentiles are what shard execution pays.
//   - waste: a batch of needed blocks plus speculative extras the
//     threshold would later kill, all issued concurrently the way a
//     planner flushes its frontier; the row's latency is still time to
//     the last needed block, so the delta against extra=0 prices
//     planner aggressiveness.
//
// Output is TSV: label arm block depth batch extra samples p50us p99us
// p999us reads_s mbps. hot and depth rows are per-op; batch and waste
// rows are per-batch. Cold arms drop the page cache before every
// config and warn on rows where that failed.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hotWindow = 256 << 20 // page-cache-resident arm window
	batchBlk  = 16 << 10  // planner-shaped arms read L1-span-sized blocks
)

func main() {
	label := flag.String("label", "local", "row label, name the host")
	path := flag.String("file", "readplanner.dat", "backing file on the disk under test")
	sizeGiB := flag.Int("size", 48, "backing file GiB; at least 2x RAM for honest cold rows")
	secs := flag.Float64("sec", 2.0, "seconds per config")
	seed := flag.Int64("seed", 2107, "rng seed")
	arms := flag.String("arms", "hot,depth,batch,waste", "comma list")
	flag.Parse()

	size := int64(*sizeGiB) << 30
	f, err := ensureFile(*path, size, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "readplanner:", err)
		os.Exit(1)
	}
	defer f.Close()

	want := map[string]bool{}
	for a := range strings.SplitSeq(*arms, ",") {
		want[a] = true
	}
	fmt.Println("label\tarm\tblock\tdepth\tbatch\textra\tsamples\tp50us\tp99us\tp999us\treads_s\tmbps")
	if want["hot"] {
		runHot(*label, f, *secs, *seed)
	}
	if want["depth"] {
		runDepth(*label, f, size, *secs, *seed)
	}
	if want["batch"] {
		runBatch(*label, f, size, *secs, *seed)
	}
	if want["waste"] {
		runWaste(*label, f, size, *secs, *seed)
	}
}

// ensureFile creates or extends the backing file with seeded random
// bytes so no filesystem can cheat with holes; an existing file of the
// right size is reused across runs.
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
			f.Close()
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	return os.Open(path)
}

// runHot measures syscall plus scheduler cost per pread with the device
// out of the loop: 4 KiB reads inside a window that a warm pass just
// pulled into the page cache.
func runHot(label string, f *os.File, secs float64, seed int64) {
	warm := make([]byte, 4<<20)
	for off := int64(0); off < hotWindow; off += int64(len(warm)) {
		f.ReadAt(warm, off)
	}
	for _, depth := range []int{1, 2, 4, 8, 16, 32} {
		lat, reads := opLoop(f, hotWindow, 4<<10, depth, secs, seed)
		report(label, "hot", 4<<10, depth, 0, 0, lat, reads, secs)
	}
}

// runDepth is the cold device envelope: random preads over the whole
// file at each (block, depth).
func runDepth(label string, f *os.File, size int64, secs float64, seed int64) {
	for _, block := range []int{4 << 10, 16 << 10, 64 << 10} {
		for _, depth := range []int{1, 2, 4, 8, 16, 32, 64} {
			dropCaches(label)
			lat, reads := opLoop(f, size, block, depth, secs, seed+int64(block+depth))
			report(label, "depth", block, depth, 0, 0, lat, reads, secs)
		}
	}
}

// runBatch measures the planner contract: B scattered blocks issued as
// one concurrent batch through a depth-D pool.
func runBatch(label string, f *os.File, size int64, secs float64, seed int64) {
	for _, b := range []int{8, 32, 128} {
		for _, depth := range []int{8, 16, 32} {
			if depth > b {
				continue
			}
			dropCaches(label)
			lat, reads := batchLoop(f, size, b, 0, depth, secs, seed+int64(b*depth))
			report(label, "batch", batchBlk, depth, b, 0, lat, reads, secs)
		}
	}
}

// runWaste re-runs the 32-needed batch with speculative extras.
func runWaste(label string, f *os.File, size int64, secs float64, seed int64) {
	const needed = 32
	for _, extra := range []int{0, 16, 32, 64} {
		dropCaches(label)
		lat, reads := batchLoop(f, size, needed, extra, 32, secs, seed+int64(extra))
		report(label, "waste", batchBlk, 32, needed, extra, lat, reads, secs)
	}
}

// opLoop runs depth goroutines in a closed loop of random aligned
// preads for secs; returns per-op latencies in microseconds and the
// total read count.
func opLoop(f *os.File, span int64, block, depth int, secs float64, seed int64) ([]float64, int) {
	var mu sync.Mutex
	var all []float64
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	var wg sync.WaitGroup
	for w := range depth {
		wg.Go(func() {
			rng := rand.New(rand.NewSource(seed + int64(w)))
			buf := make([]byte, block)
			var lat []float64
			for time.Now().Before(deadline) {
				off := alignedOffset(rng, span, block)
				t0 := time.Now()
				if _, err := f.ReadAt(buf, off); err != nil {
					fmt.Fprintln(os.Stderr, "readplanner: pread:", err)
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
	return all, len(all)
}

// batchLoop issues batches of needed+extra random block reads through a
// depth-limited pool until secs elapses; returns per-batch time to the
// last NEEDED completion in microseconds, and total reads issued.
func batchLoop(f *os.File, span int64, needed, extra, depth int, secs float64, seed int64) ([]float64, int) {
	rng := rand.New(rand.NewSource(seed))
	sem := make(chan struct{}, depth)
	bufs := make(chan []byte, depth)
	for range depth {
		bufs <- make([]byte, batchBlk)
	}
	var lat []float64
	reads := 0
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	for time.Now().Before(deadline) {
		offs := make([]int64, needed+extra)
		for i := range offs {
			offs[i] = alignedOffset(rng, span, batchBlk)
		}
		reads += len(offs)
		var neededWG, allWG sync.WaitGroup
		neededWG.Add(needed)
		allWG.Add(len(offs))
		t0 := time.Now()
		for i, off := range offs {
			go func() {
				defer allWG.Done()
				sem <- struct{}{}
				buf := <-bufs
				f.ReadAt(buf, off)
				bufs <- buf
				<-sem
				if i < needed {
					neededWG.Done()
				}
			}()
		}
		neededWG.Wait()
		lat = append(lat, float64(time.Since(t0).Nanoseconds())/1e3)
		allWG.Wait()
	}
	return lat, reads
}

// alignedOffset draws a block-aligned offset with the whole block in
// range.
func alignedOffset(rng *rand.Rand, span int64, block int) int64 {
	blocks := span / int64(block)
	return rng.Int63n(blocks) * int64(block)
}

// dropCaches asks the kernel for a cold page cache; on failure the row
// is marked so a warm-cache sweep can never pass silently as cold.
func dropCaches(label string) {
	exec.Command("sync").Run()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
		fmt.Printf("# %s: drop_caches failed (%v); rows below are NOT cold\n", label, err)
	}
	time.Sleep(200 * time.Millisecond)
}

// report prints one TSV row; reads is the total block reads behind the
// samples, which is what reads_s and mbps quote (for batch and waste
// rows that includes the speculative extras).
func report(label, arm string, block, depth, batch, extra int, lat []float64, reads int, secs float64) {
	if len(lat) == 0 {
		fmt.Printf("# %s %s block=%d depth=%d: no samples\n", label, arm, block, depth)
		return
	}
	sort.Float64s(lat)
	fmt.Printf("%s\t%s\t%d\t%d\t%d\t%d\t%d\t%.1f\t%.1f\t%.1f\t%.0f\t%.0f\n",
		label, arm, block, depth, batch, extra, len(lat),
		pct(lat, 50), pct(lat, 99), pct(lat, 99.9),
		float64(reads)/secs, float64(reads)*float64(block)/secs/1e6)
}

// pct returns the q-th percentile of sorted samples, nearest-rank.
func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}
