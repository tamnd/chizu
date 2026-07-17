package chain

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/chizu/s3c"
)

// liveBucket returns a client against the environment's bucket plus a
// prefix unique to this run, or skips.
func liveBucket(t *testing.T) (*s3c.Client, context.Context, string) {
	t.Helper()
	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "chizu-test"
	}
	c, err := s3c.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	return c, ctx, fmt.Sprintf("test/%s-%d/", t.Name(), time.Now().UnixNano())
}

// TestLiveContention runs two appenders against a real bucket and checks the
// one property everything else stands on: the chain is a dense total order
// where every staged batch lands exactly once, in per-writer order, no matter
// who wins each slot.
func TestLiveContention(t *testing.T) {
	c, ctx, prefix := liveBucket(t)

	const writers = 2
	const perWriter = 20
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for wi := range writers {
		wg.Go(func() {
			ch, err := Open(ctx, c, Options{Prefix: prefix, Writer: uint64(wi + 1), Incarnation: 1})
			if err != nil {
				errs[wi] = err
				return
			}
			for i := range perWriter {
				rec := &SegCommit{Epoch: 1, Family: FamilyPage, Partition: uint16(wi), Seq: uint64(i)}
				if _, err := ch.Append(ctx, []Record{rec}); err != nil {
					errs[wi] = fmt.Errorf("writer %d append %d: %w", wi+1, i, err)
					return
				}
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Replay the chain from zero; Open's probe walking to slot 40 proves the
	// order is dense with no gaps.
	var batches []*Batch
	r, err := Open(ctx, c, Options{Prefix: prefix, Writer: 99, Incarnation: 1,
		Observe: func(seq uint64, b *Batch) { batches = append(batches, b) }})
	if err != nil {
		t.Fatal(err)
	}
	if r.Pos() != writers*perWriter || len(batches) != writers*perWriter {
		t.Fatalf("chain holds %d batches at pos %d, want %d", len(batches), r.Pos(), writers*perWriter)
	}

	seen := map[[2]uint64]bool{}
	lastBatch := map[uint64]uint64{}
	for _, b := range batches {
		id := [2]uint64{b.Writer, b.BatchID}
		if seen[id] {
			t.Fatalf("writer %d batch %d landed twice", b.Writer, b.BatchID)
		}
		seen[id] = true
		if b.BatchID <= lastBatch[b.Writer] {
			t.Fatalf("writer %d batch %d out of order after %d", b.Writer, b.BatchID, lastBatch[b.Writer])
		}
		lastBatch[b.Writer] = b.BatchID
		if len(b.Records) != 1 {
			t.Fatalf("batch carries %d records", len(b.Records))
		}
	}
}

// TestLiveRoot runs the root lifecycle on the real bucket over both
// discovery paths: create, load, forward-only advance, racing handles.
func TestLiveRoot(t *testing.T) {
	for _, mode := range []struct {
		name string
		seq  bool
	}{{"cas", false}, {"seq", true}} {
		t.Run(mode.name, func(t *testing.T) {
			c, ctx, prefix := liveBucket(t)
			a := NewRootStore(c, prefix, 1, mode.seq)
			root := &Root{DBID: 7, CreatedMS: 1, P: 4096, ShardSize: 6000000, Frozen: []byte("law=1")}
			if err := a.Create(ctx, root); err != nil {
				t.Fatal(err)
			}
			b := NewRootStore(c, prefix, 2, mode.seq)
			if r, err := b.Load(ctx); err != nil || r.CkptSeq != 0 || r.DBID != 7 {
				t.Fatalf("load: %+v %v", r, err)
			}
			if r, err := a.Advance(ctx, 4096); err != nil || r.CkptSeq != 4096 {
				t.Fatalf("advance: %+v %v", r, err)
			}
			// b's view is stale; its lower advance must converge on 4096.
			if r, err := b.Advance(ctx, 100); err != nil || r.CkptSeq != 4096 {
				t.Fatalf("stale advance: %+v %v", r, err)
			}
			if r, err := b.Advance(ctx, 8192); err != nil || r.CkptSeq != 8192 {
				t.Fatalf("second advance: %+v %v", r, err)
			}
			final, err := a.Load(ctx)
			if err != nil || final.CkptSeq != 8192 || final.Writer != 2 || string(final.Frozen) != "law=1" {
				t.Fatalf("final: %+v %v", final, err)
			}
		})
	}
}
