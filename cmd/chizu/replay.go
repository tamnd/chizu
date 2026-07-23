// chizu replay: the fixture query log replayer of doc 07. Open-loop
// load generation: arrivals fire on a Poisson clock at the target QPS
// no matter how the server is doing, which is the only honest way to
// measure a latency distribution (closed-loop replayers slow down with
// the server and hide the tail). Queries come from a log file or are
// synthesized from a shard's own dictionary by df band.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/serve"
	"github.com/tamnd/chizu/wire"
)

func replayCmd(args []string) error {
	fs := flag.NewFlagSet("chizu replay", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:4980", "serve node address")
	qps := fs.Float64("qps", 100, "target arrival rate")
	dur := fs.Duration("sec", 10*time.Second, "run duration")
	budget := fs.Duration("budget", 10*time.Millisecond, "per-query budget")
	k := fs.Int("k", 100, "results per query")
	candidates := fs.Int("candidates", 1000, "phase-1 candidate pool")
	shard := fs.Int("shard", -1, "shard number, -1 means 0")
	logPath := fs.String("log", "", "query log file, one space-separated query per line")
	hotPath := fs.String("hot", "", "shard file to synthesize a query log from")
	n := fs.Int("n", 10_000, "synthesized log size")
	seed := fs.Int64("seed", 2107, "synthesis seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var queries [][][]byte
	var err error
	switch {
	case *logPath != "":
		queries, err = readQueryLog(*logPath)
	case *hotPath != "":
		queries, err = synthQueryLog(*hotPath, *n, *seed)
	default:
		return errors.New("replay needs -log or -hot")
	}
	if err != nil {
		return err
	}
	if len(queries) == 0 {
		return errors.New("empty query log")
	}
	sh := uint16(0)
	if *shard >= 0 {
		sh = uint16(*shard)
	}
	return replay(*addr, queries, replayConfig{
		qps: *qps, dur: *dur, budget: *budget,
		k: uint16(*k), candidates: uint16(*candidates), shard: sh,
	})
}

// readQueryLog loads a query per line, terms space-separated.
func readQueryLog(path string) ([][][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var queries [][][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var q [][]byte
		for _, t := range strings.Fields(line) {
			q = append(q, []byte(t))
		}
		queries = append(queries, q)
	}
	return queries, sc.Err()
}

// synthQueryLog samples the shard's own vocabulary by df band: head
// terms dominate arrivals the way real logs Zipf, torso fills the
// middle, tail terms keep the rare-term path warm. Query lengths draw
// from the lab 04 fixture distribution.
func synthQueryLog(path string, n int, seed int64) ([][][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sh, err := hotfmt.Open(f, st.Size())
	if err != nil {
		return nil, err
	}
	dictBand, err := sh.ReadBand(hotfmt.BandDict)
	if err != nil {
		return nil, err
	}
	d, err := hotfmt.OpenDict(dictBand)
	if err != nil {
		return nil, err
	}
	type termDF struct {
		term []byte
		df   uint32
	}
	var vocab []termDF
	if err := d.Walk(func(term []byte, e hotfmt.DictEntry) error {
		vocab = append(vocab, termDF{bytes.Clone(term), e.DF})
		return nil
	}); err != nil {
		return nil, err
	}
	if len(vocab) == 0 {
		return nil, errors.New("empty dictionary")
	}
	sort.Slice(vocab, func(i, j int) bool { return vocab[i].df > vocab[j].df })
	// Bands by rank: head is the top 1% (min 1), torso the next 20%,
	// tail the rest. Arrival mix 60/30/10.
	head := max(len(vocab)/100, 1)
	torso := max(len(vocab)/5, head+1)
	torso = min(torso, len(vocab))
	rng := rand.New(rand.NewSource(seed))
	draw := func() termDF {
		r := rng.Float64()
		switch {
		case r < 0.6:
			return vocab[rng.Intn(head)]
		case r < 0.9 && torso > head:
			return vocab[head+rng.Intn(torso-head)]
		default:
			return vocab[rng.Intn(len(vocab))]
		}
	}
	// Query length distribution from the lab 04 fixture log.
	lens := []int{1, 1, 2, 2, 2, 3, 3, 3, 4, 4, 5}
	queries := make([][][]byte, 0, n)
	for range n {
		qlen := lens[rng.Intn(len(lens))]
		seen := map[string]bool{}
		var q [][]byte
		for len(q) < qlen {
			t := draw()
			if seen[string(t.term)] {
				continue
			}
			seen[string(t.term)] = true
			q = append(q, t.term)
		}
		queries = append(queries, q)
	}
	return queries, nil
}

type replayConfig struct {
	qps        float64
	dur        time.Duration
	budget     time.Duration
	k          uint16
	candidates uint16
	shard      uint16
}

type replayOutcome struct {
	latency time.Duration
	busy    bool
	err     bool
	degrade byte
	entries int
}

// replay fires queries on a Poisson clock. Each in-flight query holds
// one connection; a lane pool reuses them so steady state does not
// churn dials, but a slow server never delays the arrival clock.
func replay(addr string, queries [][][]byte, cfg replayConfig) error {
	lanes := make(chan *serve.Client, 4096)
	dial := func() (*serve.Client, error) {
		select {
		case c := <-lanes:
			return c, nil
		default:
			return serve.Dial(addr, 999)
		}
	}
	// Warm check: the node is up before the clock starts.
	c, err := serve.Dial(addr, 999)
	if err != nil {
		return err
	}
	if err := c.Ping(); err != nil {
		return err
	}
	lanes <- c

	rng := rand.New(rand.NewSource(1))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var outcomes []replayOutcome
	start := time.Now()
	next := start
	fired := 0
	for {
		now := time.Now()
		if now.Sub(start) >= cfg.dur {
			break
		}
		if now.Before(next) {
			time.Sleep(next.Sub(now))
		}
		// Exponential inter-arrival: the Poisson clock.
		next = next.Add(time.Duration(rng.ExpFloat64() / cfg.qps * float64(time.Second)))
		q := queries[fired%len(queries)]
		fired++
		wg.Add(1)
		go func(terms [][]byte) {
			defer wg.Done()
			var out replayOutcome
			cl, err := dial()
			if err != nil {
				out.err = true
				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()
				return
			}
			wq := &wire.Query{
				BudgetUS:   uint32(cfg.budget.Microseconds()),
				K:          cfg.k,
				Candidates: cfg.candidates,
				Shards:     []uint16{cfg.shard},
			}
			for _, t := range terms {
				wq.Terms = append(wq.Terms, wire.QueryTerm{Term: t})
			}
			t0 := time.Now()
			r, busy, err := cl.Query(wq)
			out.latency = time.Since(t0)
			switch {
			case err != nil:
				out.err = true
				_ = cl.Close()
			case busy:
				out.busy = true
				lanes <- cl
			default:
				out.degrade = r.Degrade
				out.entries = len(r.Entries)
				select {
				case lanes <- cl:
				default:
					_ = cl.Close()
				}
			}
			mu.Lock()
			outcomes = append(outcomes, out)
			mu.Unlock()
		}(q)
	}
	wg.Wait()
	report(outcomes, fired, cfg)
	return nil
}

// report prints the full distribution, not a summary statistic: the
// deadline contract lives at p99.9, so that row is the verdict row.
func report(outcomes []replayOutcome, fired int, cfg replayConfig) {
	var ok, busy, errs, degraded int
	var lats []time.Duration
	for _, o := range outcomes {
		switch {
		case o.err:
			errs++
		case o.busy:
			busy++
		default:
			ok++
			lats = append(lats, o.latency)
			if o.degrade != 0 {
				degraded++
			}
		}
	}
	fmt.Printf("fired %d, answered %d, busy %d, errors %d, degraded %d\n",
		fired, ok, busy, errs, degraded)
	if len(lats) == 0 {
		return
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	pct := func(p float64) time.Duration {
		i := int(math.Ceil(p/100*float64(len(lats)))) - 1
		return lats[max(i, 0)]
	}
	fmt.Printf("latency  p50 %v  p90 %v  p99 %v  p99.9 %v  max %v\n",
		pct(50), pct(90), pct(99), pct(99.9), lats[len(lats)-1])
	over := 0
	for _, l := range lats {
		if l > cfg.budget {
			over++
		}
	}
	fmt.Printf("over budget (%v): %d of %d (%.3f%%)\n",
		cfg.budget, over, len(lats), 100*float64(over)/float64(len(lats)))
}
