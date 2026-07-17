// Chain-rate: measures append throughput, 412 re-read behavior, and the
// batch-size lever at chizu's design record mix (spec 2107 doc 02 section 4).
//
// One process drives N contending appenders against the bucket named by the
// CHIZU_S3_* environment. Every append is one batch of -records records
// shaped like the design mix: seg-commits with a heartbeat riding along and
// the occasional manifest advance and lease renewal, which is what a crawl
// node's append loop stages in real life. -pace throttles each contender to
// a fixed append rate; 0 saturates.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/s3c"
)

func main() {
	contenders := flag.Int("contenders", 3, "concurrent appenders")
	records := flag.Int("records", 8, "records per append")
	duration := flag.Duration("duration", 30*time.Second, "run length")
	pace := flag.Float64("pace", 0, "appends per second per contender; 0 saturates")
	prefix := flag.String("prefix", "", "key prefix; default lab/chainrate-<nanos>/")
	label := flag.String("label", "", "arm label echoed in the result row")
	flag.Parse()
	if err := run(*contenders, *records, *duration, *pace, *prefix, *label); err != nil {
		fmt.Fprintln(os.Stderr, "chainrate:", err)
		os.Exit(1)
	}
}

type contender struct {
	appends  int
	rereads  int
	failures int
	lat      []time.Duration
}

func run(contenders, records int, duration time.Duration, pace float64, prefix, label string) error {
	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		return fmt.Errorf("CHIZU_S3_ENDPOINT is unset")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "chizu-lab"
	}
	if prefix == "" {
		prefix = fmt.Sprintf("lab/chainrate-%d/", time.Now().UnixNano())
	}
	setup, err := s3c.New(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := setup.CreateBucket(ctx); err != nil {
		return err
	}

	results := make([]contender, contenders)
	errs := make([]error, contenders)
	start := time.Now()
	deadline := start.Add(duration)
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// Paced contenders are staggered across the period; real nodes are not
	// phase-locked, and launching them on the same tick would collide every
	// append by construction.
	var stagger time.Duration
	if pace > 0 {
		stagger = time.Duration(float64(time.Second) / pace / float64(contenders))
	}
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Go(func() {
			errs[i] = drive(ctx, runCtx, cfg, prefix, uint64(i+1), records, pace, time.Duration(i)*stagger, deadline, &results[i])
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	var total contender
	for i, r := range results {
		total.appends += r.appends
		total.rereads += r.rereads
		total.failures += r.failures
		total.lat = append(total.lat, r.lat...)
		fmt.Printf("  [c%02d] appends=%d rereads=%d failures=%d\n", i+1, r.appends, r.rereads, r.failures)
	}
	slices.Sort(total.lat)
	secs := elapsed.Seconds()
	rate := 0.0
	if total.appends > 0 {
		rate = float64(total.rereads) / float64(total.appends)
	}
	fmt.Printf("RESULT label=%s contenders=%d records=%d pace=%.2f dur=%.1fs appends=%d appends_per_s=%.1f records_per_s=%.1f rereads=%d reread_rate=%.1f%% failures=%d p50=%s p95=%s p99=%s max=%s\n",
		label, contenders, records, pace, secs,
		total.appends, float64(total.appends)/secs, float64(total.appends*records)/secs,
		total.rereads, 100*rate, total.failures,
		pctl(total.lat, 0.50), pctl(total.lat, 0.95), pctl(total.lat, 0.99), pctl(total.lat, 1.0))
	return nil
}

// drive is one contender: its own client, its own chain handle, one append
// loop. Re-reads are counted through Observe: every foreign batch the handle
// folds mid-append is one slot it lost and had to read. Appends run on
// runCtx, which expires at the deadline, so a contender starving in the
// 412 retry loop stops on time instead of spinning past the window.
func drive(openCtx, runCtx context.Context, cfg s3c.Config, prefix string, writer uint64, records int, pace float64, offset time.Duration, deadline time.Time, out *contender) error {
	client, err := s3c.New(cfg)
	if err != nil {
		return err
	}
	foreign := 0
	ch, err := chain.Open(openCtx, client, chain.Options{
		Prefix: prefix, Writer: writer, Incarnation: 1,
		Observe: func(seq uint64, b *chain.Batch) {
			if b.Writer != writer {
				foreign++
			}
		},
	})
	if err != nil {
		return err
	}

	var interval time.Duration
	if pace > 0 {
		interval = time.Duration(float64(time.Second) / pace)
	}
	next := time.Now().Add(offset)
	for k := uint64(0); ; k++ {
		if pace > 0 {
			time.Sleep(time.Until(next))
			next = next.Add(interval)
			// A real node rides the chain between its appends, so a paced
			// contender folds up to the tail first; foreign batches seen
			// here are ordinary polling, not 412 costs. Saturated
			// contenders skip this: the CAS race itself is their story.
			if err := ch.Poll(runCtx); err != nil && runCtx.Err() == nil {
				return err
			}
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		before := foreign
		t0 := time.Now()
		_, err := ch.Append(runCtx, designMix(records, k, writer))
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			out.failures++
			continue
		}
		out.lat = append(out.lat, time.Since(t0))
		out.appends++
		out.rereads += foreign - before
	}
}

// designMix shapes one batch like the doc 02 section 4 rate table: mostly
// seg-commits, a member heartbeat riding along, a manifest advance roughly
// every eighth append, and a lease renewal roughly every sixteenth.
func designMix(n int, k, writer uint64) []chain.Record {
	part := uint16(k % 64)
	recs := make([]chain.Record, 0, n)
	if n > 1 {
		recs = append(recs, &chain.Member{Node: writer, Plane: chain.PlaneCrawl, Incarnation: 1,
			Endpoints: []string{"10.0.0.1:7070"}, Version: "lab"})
	}
	if n > 2 && k%8 == 0 {
		recs = append(recs, &chain.ManAdvance{Epoch: 1, Family: chain.FamilyPage, Partition: part, ManSeq: k})
	}
	if n > 2 && k%16 == 0 {
		recs = append(recs, &chain.LeaseGrant{Epoch: 1, Domain: chain.DomainCrawlPart, Unit: uint32(part),
			Node: writer, DeadlineMS: (k + 60) * 1000})
	}
	for len(recs) < n {
		recs = append(recs, &chain.SegCommit{Epoch: 1, Family: chain.FamilyPage, Partition: part,
			Seq: k, Rows: 20_000, Bytes: 64 << 20, Watermark: k})
	}
	return recs
}

func pctl(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := max(int(q*float64(len(sorted)))-1, 0)
	return sorted[min(i, len(sorted)-1)].Round(time.Millisecond)
}
