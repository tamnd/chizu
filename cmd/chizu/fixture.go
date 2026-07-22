package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tamnd/chizu/fixture"
)

// fixtureCmd writes a versioned fixture artifact as cold page segments
// under a local directory, mirroring the bucket key shape
// (cold/page/p0000/<seq>.cold), plus a FIXTURE.txt identity file whose
// digest chains every segment byte. The same (version, seed, n) always
// reproduces the same bytes; that is what makes the artifact versioned
// rather than stored.
func fixtureCmd(args []string) error {
	fs := flag.NewFlagSet("chizu fixture", flag.ContinueOnError)
	n := fs.Uint64("n", 10_000_000, "pages to generate")
	seed := fs.Uint64("seed", 2107, "generator seed")
	rows := fs.Int("rows", 1<<16, "pages per segment")
	dir := fs.String("dir", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("chizu fixture: -dir is required")
	}

	c := fixture.New(*seed, *n)
	segDir := filepath.Join(*dir, "cold", "page", "p0000")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return err
	}

	digest := sha256.New()
	var total, segs uint64
	start := time.Now()
	err := c.Segments(*seed, *rows, func(seq uint64, seg []byte) error {
		digest.Write(seg)
		total += uint64(len(seg))
		segs = seq
		name := filepath.Join(segDir, fmt.Sprintf("%016x.cold", seq))
		if err := os.WriteFile(name, seg, 0o644); err != nil {
			return err
		}
		if seq%16 == 0 {
			el := time.Since(start).Seconds()
			done := seq * uint64(*rows)
			fmt.Printf("%s: %d pages, %.1f MB, %.0f pages/s\n", c.Name(), done, float64(total)/1e6, float64(done)/el)
		}
		return nil
	})
	if err != nil {
		return err
	}

	sum := hex.EncodeToString(digest.Sum(nil))
	id := fmt.Sprintf("name %s\nversion %d\nseed %d\npages %d\nrows-per-segment %d\nsegments %d\nbytes %d\nsha256 %s\n",
		c.Name(), fixture.Version, *seed, *n, *rows, segs, total, sum)
	if err := os.WriteFile(filepath.Join(*dir, "FIXTURE.txt"), []byte(id), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d pages in %d segments, %.1f MB, sha256 %s, %.0fs\n",
		c.Name(), *n, segs, float64(total)/1e6, sum, time.Since(start).Seconds())
	return nil
}
