package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/s3c"
	"github.com/tamnd/chizu/tokenize"
)

// emitRows widens testRows so the merge crosses FOR-block boundaries:
// "water" lands in 301 docs (three postings blocks) and the zebra doc
// overflows the u8 tf.
func emitRows(t *testing.T) []coldfmt.PageRow {
	t.Helper()
	rows := testRows(t)
	page := func(url, title, text string) coldfmt.PageRow {
		fp := sha256.Sum256([]byte(url))
		r := coldfmt.PageRow{
			URL: url, Title: title, Text: text,
			FetchMS: 1_750_000_000_000,
			Status:  200, Lang: 1, LawVer: 1,
			SHA256: sha256.Sum256([]byte(text)),
		}
		copy(r.URLFP[:], fp[:16])
		r.CanonFP = r.URLFP
		return r
	}
	for i := range 300 {
		rows = append(rows, page(
			fmt.Sprintf("https://t.chizu/w%03d", i),
			fmt.Sprintf("Water report %d", i),
			fmt.Sprintf("water level w%d rises in region r%d water", i, i%7),
		))
	}
	rows = append(rows, page("https://t.chizu/zebra", "Zebra",
		strings.TrimSpace(strings.Repeat("zebra ", 300))))
	return rows
}

func emitConfig(t *testing.T) *EmitConfig {
	t.Helper()
	return &EmitConfig{
		SpoolDir:   t.TempDir(),
		Shard:      0,
		Generation: 1,
		Writer:     1,
		Builder:    1,
		BuildMS:    1_750_000_000_000,
		Watermarks: []uint64{1},
	}
}

func emitShard(t *testing.T, rows []coldfmt.PageRow, budget int) []byte {
	t.Helper()
	out := runPass(t, t.TempDir(), rows, budget)
	var buf bytes.Buffer
	if err := Emit(&buf, out, emitConfig(t)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEmitMatchesBruteForce(t *testing.T) {
	rows := emitRows(t)
	data := emitShard(t, rows, 8<<10)

	sh, err := hotfmt.Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.VerifyBands(); err != nil {
		t.Fatal(err)
	}
	want := brute(rows)
	if sh.Header.DocCount != uint32(len(rows)) {
		t.Fatalf("doc count %d want %d", sh.Header.DocCount, len(rows))
	}
	if sh.Header.TermCount != uint64(len(want)) {
		t.Fatalf("term count %d want %d", sh.Header.TermCount, len(want))
	}
	if sh.Header.TokenizerVer != tokenize.Version {
		t.Fatalf("tokenizer version %d", sh.Header.TokenizerVer)
	}

	dictBand, err := sh.ReadBand(hotfmt.BandDict)
	if err != nil {
		t.Fatal(err)
	}
	dict, err := hotfmt.OpenDict(dictBand)
	if err != nil {
		t.Fatal(err)
	}
	postingsBand, err := sh.ReadBand(hotfmt.BandPostings)
	if err != nil {
		t.Fatal(err)
	}
	skipsBand, err := sh.ReadBand(hotfmt.BandSkips)
	if err != nil {
		t.Fatal(err)
	}
	positionsBand, err := sh.ReadBand(hotfmt.BandPositions)
	if err != nil {
		t.Fatal(err)
	}

	sawBlocks := 0
	for term, docs := range want {
		e, ok, err := dict.Lookup([]byte(term))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("term %q missing from dictionary", term)
		}
		if int(e.DF) != len(docs) {
			t.Fatalf("%q df %d want %d", term, e.DF, len(docs))
		}
		if e.DF <= 4 {
			for _, p := range e.Inline {
				bp := docs[p.Docid]
				if bp == nil {
					t.Fatalf("%q inline doc %d unexpected", term, p.Docid)
				}
				if uint32(p.TF) != min(bp.tf, 255) || p.Mask != bp.mask {
					t.Fatalf("%q inline doc %d: tf %d mask %d, want tf %d mask %d",
						term, p.Docid, p.TF, p.Mask, min(bp.tf, 255), bp.mask)
				}
			}
			continue
		}

		var cf uint64
		for _, bp := range docs {
			cf += uint64(bp.tf)
		}
		if e.CF != cf {
			t.Fatalf("%q cf %d want %d", term, e.CF, cf)
		}
		termBytes := postingsBand[e.PostingsOff : e.PostingsOff+uint64(e.PostingsLen)]
		region := skipsBand[e.SkipOff : e.SkipOff+uint64(hotfmt.SkipRegionSize(e.DF))]
		l1, posOffs, _, err := hotfmt.ParseSkips(region, e.DF)
		if err != nil {
			t.Fatalf("%q: %v", term, err)
		}
		var docids [hotfmt.PostingsBlockLen]uint32
		var tfs, masks [hotfmt.PostingsBlockLen]uint8
		seen := 0
		for bi := range l1 {
			sawBlocks++
			prev := int64(-1)
			if bi > 0 {
				prev = int64(l1[bi-1].LastDocid)
			}
			b, _, err := hotfmt.DecodePostingsBlock(termBytes[l1[bi].Off:], prev, docids[:], tfs[:], masks[:])
			if err != nil {
				t.Fatalf("%q block %d: %v", term, bi, err)
			}
			posOff := posOffs[bi]
			for i := range b.NEntries {
				bp := docs[docids[i]]
				if bp == nil {
					t.Fatalf("%q doc %d unexpected", term, docids[i])
				}
				if uint32(tfs[i]) != min(bp.tf, 255) || masks[i] != bp.mask {
					t.Fatalf("%q doc %d: tf %d mask %d, want tf %d mask %d",
						term, docids[i], tfs[i], masks[i], min(bp.tf, 255), bp.mask)
				}
				fields, n, err := hotfmt.DecodePositionRun(positionsBand[posOff:], masks[i])
				if err != nil {
					t.Fatalf("%q doc %d positions: %v", term, docids[i], err)
				}
				posOff += uint64(n)
				for _, fp := range fields {
					wantPos := bp.pos[fp.Field]
					if len(fp.Positions) != len(wantPos) {
						t.Fatalf("%q doc %d field %d positions %v want %v",
							term, docids[i], fp.Field, fp.Positions, wantPos)
					}
					for j := range fp.Positions {
						if fp.Positions[j] != wantPos[j] {
							t.Fatalf("%q doc %d field %d positions %v want %v",
								term, docids[i], fp.Field, fp.Positions, wantPos)
						}
					}
				}
				seen++
			}
		}
		if seen != len(docs) {
			t.Fatalf("%q walked %d postings, want %d", term, seen, len(docs))
		}
	}
	if sawBlocks < 3 {
		t.Fatalf("walked %d FOR blocks, want a multi-block term", sawBlocks)
	}

	// The doc band and docvalues ride through untouched.
	docBand, err := sh.ReadBand(hotfmt.BandDocband)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := hotfmt.OpenDocBand(docBand)
	if err != nil {
		t.Fatal(err)
	}
	defer docs.Close()
	rec, err := docs.Doc(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.URL) != rows[0].URL {
		t.Fatalf("doc 0 url %s", rec.URL)
	}
}

func TestEmitTFCap(t *testing.T) {
	rows := emitRows(t)
	data := emitShard(t, rows, 8<<10)
	sh, err := hotfmt.Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	dictBand, err := sh.ReadBand(hotfmt.BandDict)
	if err != nil {
		t.Fatal(err)
	}
	dict, err := hotfmt.OpenDict(dictBand)
	if err != nil {
		t.Fatal(err)
	}
	e, ok, err := dict.Lookup([]byte("zebra"))
	if err != nil || !ok {
		t.Fatalf("zebra lookup: %v %v", ok, err)
	}
	if len(e.Inline) != 1 || e.Inline[0].TF != 255 {
		t.Fatalf("zebra entry %+v: tf must cap at 255", e)
	}
}

func TestEmitDeterministic(t *testing.T) {
	rows := emitRows(t)
	a := emitShard(t, rows, 8<<10)
	b := emitShard(t, rows, 8<<10)
	if !bytes.Equal(a, b) {
		t.Fatal(".hot bytes differ across builds")
	}
}

// A field longer than 65536 tokens clamps overflow occurrences to the
// 65535 sentinel, so every mask bit keeps a stored position and the
// emit pass can always encode the run.
func TestEmitPositionOverflow(t *testing.T) {
	fp := sha256.Sum256([]byte("https://t.chizu/long"))
	row := coldfmt.PageRow{
		URL: "https://t.chizu/long", Title: "Long",
		Text:    strings.TrimSpace(strings.Repeat("pad ", 70_000)) + " omega",
		FetchMS: 1_750_000_000_000,
		Status:  200, Lang: 1, LawVer: 1,
	}
	row.SHA256 = sha256.Sum256([]byte(row.Text))
	copy(row.URLFP[:], fp[:16])
	row.CanonFP = row.URLFP

	out := runPass(t, t.TempDir(), []coldfmt.PageRow{row}, 0)
	r, err := OpenRun(out.Runs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var rec Rec
	sawOmega := false
	for {
		if err := r.Next(&rec); err != nil {
			break
		}
		if string(rec.Term) == "omega" {
			sawOmega = true
			if len(rec.Pos[fieldBody]) != 1 || rec.Pos[fieldBody][0] != 0xFFFF {
				t.Fatalf("omega positions %v, want the 65535 sentinel", rec.Pos[fieldBody])
			}
		}
		if string(rec.Term) == "pad" {
			if got := len(rec.Pos[fieldBody]); got != 65536 {
				t.Fatalf("pad keeps %d positions, want 65536", got)
			}
		}
	}
	if !sawOmega {
		t.Fatal("omega record missing")
	}

	var buf bytes.Buffer
	if err := Emit(&buf, out, emitConfig(t)); err != nil {
		t.Fatalf("emit rejects the sentinel shard: %v", err)
	}
}

func TestUploadHot(t *testing.T) {
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

	data := emitShard(t, testRows(t), 0)
	path := filepath.Join(t.TempDir(), "shard.hot")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("emit-test/%d/shard.hot", os.Getpid())
	if err := UploadHot(ctx, c, key, path, minPartSize); err != nil {
		t.Fatal(err)
	}
	got, _, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("uploaded shard bytes differ")
	}
	if err := UploadHot(ctx, c, key, path, 1<<20); err == nil {
		t.Fatal("part size below the S3 minimum accepted")
	}
}
