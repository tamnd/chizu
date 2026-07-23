package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tamnd/chizu/build"
	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/fixture"
	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/s3c"
	"github.com/tamnd/chizu/tokenize"
)

// The fixture vertical: `chizu dev -fixture N` builds an N-doc fixture
// shard through the real pipeline, reopens the local file, answers a
// hand-rolled term lookup from it, then uploads it with the consuming
// path. This is the C1a exit-gate shape (doc 10 section 1): the corpus
// enters the build only as sealed .cold segment bytes, the .hot lives
// on disk end to end, and the lookup preads postings straight from the
// file, so RAM holds the dictionary and docvalues and nothing else.
// Disk peaks at one shard copy: the merge punches consumed run
// prefixes and the upload punches acknowledged parts.

const (
	devFixtureSeed    = 2107
	devFixtureRowsSeg = 10_000
)

func devFixture(ctx context.Context, client *s3c.Client, prefix, scratch string, n uint64, runBudget int) error {
	dir := scratch
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "chizu-dev-fixture-"); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
	}

	c := fixture.New(devFixtureSeed, n)
	hotPath, err := devFixtureBuild(dir, c, devFixtureRowsSeg, runBudget)
	if err != nil {
		return err
	}
	st, err := os.Stat(hotPath)
	if err != nil {
		return err
	}
	sum, err := fileSHA256(hotPath)
	if err != nil {
		return err
	}
	fmt.Printf("fixture build: %d docs -> %s (%d bytes, sha256 %x)\n", n, hotPath, st.Size(), sum)

	// Verify and serve from the scratch file first, then upload with
	// the consuming path: the bucket copy grows as the scratch copy's
	// blocks are punched away, so a shard's disk cost peaks at one
	// copy, not two. At 100M docs on the gate box that is the
	// difference between fitting and not.
	if err := devFixtureCheck(hotPath, c, n); err != nil {
		return err
	}
	shardKey := prefix + "hot/s0000/0000000000000001.hot"
	if err := build.UploadHotConsume(ctx, client, shardKey, hotPath, build.DefaultPartSize); err != nil {
		return err
	}
	if err := os.Remove(hotPath); err != nil {
		return err
	}
	fmt.Printf("fixture upload: %s (scratch copy consumed)\n", shardKey)
	return nil
}

// devFixtureBuild streams the corpus through sealed cold segments into
// the shard pass and emits the .hot to a file in dir. Segments live
// only as in-memory bytes, one at a time; the build never sees a page
// except through coldfmt.
func devFixtureBuild(dir string, c *fixture.Corpus, rowsPerSeg, runBudget int) (string, error) {
	p := build.NewShardPass(dir, runBudget)
	var watermark uint64
	var built uint64
	err := c.Segments(1, rowsPerSeg, func(seq uint64, seg []byte) error {
		ps, err := coldfmt.OpenPageSegment(seg)
		if err != nil {
			return err
		}
		rows, err := ps.Rows()
		ps.Close()
		if err != nil {
			return err
		}
		for i := range rows {
			watermark = max(watermark, rows[i].FetchMS)
			if err := p.AddRow(&rows[i]); err != nil {
				return err
			}
		}
		before := built
		built += uint64(len(rows))
		if built/1_000_000 != before/1_000_000 {
			fmt.Printf("  shard pass: %d docs\n", built)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	out, err := p.Finish()
	if err != nil {
		return "", err
	}

	hotPath := filepath.Join(dir, "shard.hot")
	f, err := os.Create(hotPath)
	if err != nil {
		return "", err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	cfg := &build.EmitConfig{
		SpoolDir:   dir,
		Shard:      0,
		Generation: 1,
		Writer:     1,
		Builder:    1,
		BuildMS:    watermark,
		Watermarks: []uint64{watermark},
	}
	if err := build.Emit(bw, out, cfg); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hotPath, nil
}

// devFixtureCheck reopens the .hot file-backed, verifies every band
// crc (H-I2), and walks probe terms from the dictionary through their
// postings blocks with per-block preads, so a block is located by its
// skip entry alone and decoded standalone (H-I3).
func devFixtureCheck(hotPath string, c *fixture.Corpus, n uint64) error {
	f, err := os.Open(hotPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	sh, err := hotfmt.Open(f, st.Size())
	if err != nil {
		return err
	}
	if err := sh.VerifyBands(); err != nil {
		return err
	}
	if uint64(sh.Header.DocCount) != n {
		return fmt.Errorf("dev fixture: shard holds %d docs, corpus has %d", sh.Header.DocCount, n)
	}

	dictBand, err := sh.ReadBand(hotfmt.BandDict)
	if err != nil {
		return err
	}
	dict, err := hotfmt.OpenDict(dictBand)
	if err != nil {
		return err
	}
	dvBand, err := sh.ReadBand(hotfmt.BandDocvalues)
	if err != nil {
		return err
	}
	dv, err := hotfmt.OpenDocValues(dvBand, sh.Header.DocCount)
	if err != nil {
		return err
	}
	postOff, _, err := sh.Band(hotfmt.BandPostings)
	if err != nil {
		return err
	}
	skipOff, _, err := sh.Band(hotfmt.BandSkips)
	if err != nil {
		return err
	}

	for _, term := range devFixtureProbes(c, n) {
		e, ok, err := dict.Lookup(term)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("dev fixture: probe term %q missing from the dictionary", term)
		}
		var firstDoc uint32
		var blocks int
		if len(e.Inline) > 0 {
			firstDoc = e.Inline[0].Docid
		} else {
			region := make([]byte, hotfmt.SkipRegionSize(e.DF))
			if _, err := f.ReadAt(region, int64(skipOff+e.SkipOff)); err != nil {
				return err
			}
			l1, _, _, err := hotfmt.ParseSkips(region, e.DF)
			if err != nil {
				return err
			}
			var docids [hotfmt.PostingsBlockLen]uint32
			var tfs, masks [hotfmt.PostingsBlockLen]uint8
			var seen uint32
			last := int64(-1)
			for bi := range l1 {
				end := uint64(e.PostingsLen)
				if bi+1 < len(l1) {
					end = l1[bi+1].Off
				}
				blk := make([]byte, end-l1[bi].Off)
				if _, err := f.ReadAt(blk, int64(postOff+e.PostingsOff+l1[bi].Off)); err != nil {
					return err
				}
				prev := int64(-1)
				if bi > 0 {
					prev = int64(l1[bi-1].LastDocid)
				}
				b, _, err := hotfmt.DecodePostingsBlock(blk, prev, docids[:], tfs[:], masks[:])
				if err != nil {
					return err
				}
				for i := range b.NEntries {
					if int64(docids[i]) <= last {
						return fmt.Errorf("dev fixture: term %q docids not increasing", term)
					}
					last = int64(docids[i])
					if seen == 0 {
						firstDoc = docids[i]
					}
					seen++
				}
				blocks++
			}
			if seen != e.DF {
				return fmt.Errorf("dev fixture: term %q walked %d postings, df says %d", term, seen, e.DF)
			}
		}
		if dv.At(firstDoc).DoclenBody == 0 {
			return fmt.Errorf("dev fixture: doc %d has a zero body length bucket", firstDoc)
		}
		fmt.Printf("  lookup %q: df %d, %d blocks\n", term, e.DF, blocks)
	}
	fmt.Printf("fixture lookup: file-backed shard, bands verified, %d docs\n", sh.Header.DocCount)
	return nil
}

// devFixtureProbes tokenizes the first and last fixture pages and
// takes the first unique admitted terms of each, so the probes span
// head-of-Zipf terms and a rare tail term without hardcoding the
// generator's vocabulary.
func devFixtureProbes(c *fixture.Corpus, n uint64) [][]byte {
	var probes [][]byte
	var stats tokenize.Stats
	collect := func(docid uint64, k int) {
		page := c.Page(docid)
		before := len(probes)
		tokenize.Text(page.Text, &stats, func(term []byte, pos uint32) {
			if len(probes)-before == k {
				return
			}
			for _, p := range probes {
				if bytes.Equal(p, term) {
					return
				}
			}
			probes = append(probes, bytes.Clone(term))
		})
	}
	collect(0, 4)
	collect(n-1, 2)
	return probes
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
