package chain

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/chizu/s3c"
)

// TestLiveContention runs two appenders against a real bucket and checks the
// one property everything else stands on: the chain is a dense total order
// where every staged batch lands exactly once, in per-writer order, no matter
// who wins each slot.
func TestLiveContention(t *testing.T) {
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
	defer cancel()
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("test/chain-%d/", time.Now().UnixNano())

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
