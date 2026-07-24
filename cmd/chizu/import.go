package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/crawl"
	"github.com/tamnd/chizu/s3c"
)

// importCmd runs the Common Crawl importer (doc 03 section 13): WET
// sources stream through canonicalize/dedup/store into cold page
// segments, each commit landing on the chain. Sources are local
// .wet.gz paths or https:// URLs against the public bucket.
func importCmd(args []string) error {
	fs := flag.NewFlagSet("chizu import", flag.ContinueOnError)
	prefix := fs.String("prefix", "dev/", "database key prefix inside the bucket")
	wet := fs.String("wet", "", "comma list of WET sources (.wet.gz paths or https URLs)")
	parts := fs.Uint("parts", 4, "partitions in the synthetic crawl set")
	epoch := fs.Uint("epoch", 1, "writer epoch stamped into segments and commits")
	writer := fs.Uint64("writer", 1, "writer id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *wet == "" {
		return errors.New("import needs -wet")
	}
	cfg := s3c.FromEnv()
	if cfg.Endpoint == "" {
		return errors.New("CHIZU_S3_ENDPOINT is unset; import needs a bucket from the CHIZU_S3_* environment")
	}
	client, err := s3c.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	if err := client.CreateBucket(ctx); err != nil {
		return err
	}
	ch, err := chain.Open(ctx, client, chain.Options{
		Prefix: *prefix, Writer: *writer, Incarnation: 1,
	})
	if err != nil {
		return err
	}
	im := &crawl.Importer{
		Parts:  uint16(*parts),
		Epoch:  uint32(*epoch),
		Writer: *writer,
		Sink: &crawl.BucketSink{
			Client: client, Chain: ch, Prefix: *prefix, Epoch: uint32(*epoch),
		},
	}
	start := time.Now()
	for src := range strings.SplitSeq(*wet, ",") {
		if err := importSource(ctx, im, src); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		fmt.Printf("import: %s done, %d pages so far\n", src, im.Stats.Pages)
	}
	if err := im.Flush(ctx); err != nil {
		return err
	}
	s := im.Stats
	fmt.Printf("import: %d pages, %d dups folded, %d rejected, %d short, %d segments in %s\n",
		s.Pages, s.Dups, s.Rejected, s.Short, s.Segments, time.Since(start).Round(time.Second))
	return nil
}

func importSource(ctx context.Context, im *crawl.Importer, src string) error {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %s", resp.Status)
		}
		return im.ImportWET(ctx, resp.Body)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return im.ImportWET(ctx, f)
}
