package crawl

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/s3c"
)

// TestBucketSink runs one small import against a real bucket and checks
// the doc 04 landing: object at cold/page/p<pppp>/<seq16>.cold, then a
// SegCommit on the chain carrying the same numbers.
func TestBucketSink(t *testing.T) {
	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		t.Skip("CHIZU_S3_ENDPOINT unset; the s3-suite lane provides MinIO")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "chizu-test"
	}
	client, err := s3c.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := client.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("test/%s-%d/", t.Name(), time.Now().UnixNano())

	var commits []*chain.SegCommit
	ch, err := chain.Open(ctx, client, chain.Options{
		Prefix: prefix, Writer: 1, Incarnation: 1,
		Observe: func(_ uint64, b *chain.Batch) {
			for _, rec := range b.Records {
				if sc, ok := rec.(*chain.SegCommit); ok {
					commits = append(commits, sc)
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	im := &Importer{
		Parts: 2, Epoch: 3, Writer: 1,
		Sink: &BucketSink{Client: client, Chain: ch, Prefix: prefix, Epoch: 3},
	}
	wet := gzWET(
		wetRecord("https://a.example/one", "2026-06-05T21:48:11Z", body("one")),
		wetRecord("https://b.example/two", "2026-06-05T21:48:12Z", body("two")),
	)
	if err := im.ImportWET(ctx, bytes.NewReader(wet)); err != nil {
		t.Fatal(err)
	}
	if err := im.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if im.Stats.Pages != 2 || im.Stats.Segments == 0 {
		t.Fatalf("stats = %+v", im.Stats)
	}
	if len(commits) != int(im.Stats.Segments) {
		t.Fatalf("%d SegCommit records for %d segments", len(commits), im.Stats.Segments)
	}
	for _, sc := range commits {
		if sc.Epoch != 3 || sc.Family != chain.FamilyPage {
			t.Fatalf("commit = %+v", sc)
		}
		key := fmt.Sprintf("%scold/page/p%04d/%016d.cold", prefix, sc.Partition, sc.Seq)
		data, _, err := client.Get(ctx, key)
		if err != nil {
			t.Fatalf("committed segment missing at %s: %v", key, err)
		}
		if uint64(len(data)) != sc.Bytes {
			t.Fatalf("object is %d bytes, commit says %d", len(data), sc.Bytes)
		}
		seg, err := coldfmt.OpenPageSegment(data)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := seg.Rows()
		seg.Close()
		if err != nil {
			t.Fatal(err)
		}
		if uint64(len(rows)) != sc.Rows {
			t.Fatalf("segment has %d rows, commit says %d", len(rows), sc.Rows)
		}
	}
}
