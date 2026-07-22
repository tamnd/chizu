package build

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/hotfmt"
	"github.com/tamnd/chizu/tokenize"
)

func testRows(t *testing.T) []coldfmt.PageRow {
	t.Helper()
	pages := []struct{ url, title, text string }{
		{"https://t.chizu/fuji", "Mount Fuji", "fuji is the tallest mountain in japan and a sacred mountain"},
		{"https://t.chizu/alps", "The Alps", "the alps mountain range crosses europe"},
		{"https://t.chizu/andes", "Andes Mountain Chain", "the andes mountain chain feeds every river with mountain meltwater"},
		{"https://t.chizu/amazon", "Amazon River", "the amazon river carries more water than any other river. " + strings.Repeat("basin flood plain forest water ", 60)},
		{"https://t.chizu/tokyo", "東京", "東京は日本の首都です"},
	}
	rows := make([]coldfmt.PageRow, len(pages))
	for i, p := range pages {
		fp := sha256.Sum256([]byte(p.url))
		rows[i] = coldfmt.PageRow{
			URL: p.url, Title: p.title, Text: p.text,
			FetchMS: 1_750_000_000_000 + uint64(i),
			Status:  200, Lang: 1, LawVer: 1,
			SHA256: sha256.Sum256([]byte(p.text)),
		}
		copy(rows[i].URLFP[:], fp[:16])
		rows[i].CanonFP = rows[i].URLFP
	}
	return rows
}

// brute recomputes the postings a shard pass should produce, straight
// from the tokenizer.
type brutePosting struct {
	tf   uint32
	mask uint8
	pos  [NumFields][]uint16
}

func brute(rows []coldfmt.PageRow) map[string]map[uint32]*brutePosting {
	post := make(map[string]map[uint32]*brutePosting)
	var st tokenize.Stats
	for docid, row := range rows {
		for f, text := range []string{row.Text, row.Title} {
			tokenize.Text(text, &st, func(term []byte, pos uint32) {
				m := post[string(term)]
				if m == nil {
					m = make(map[uint32]*brutePosting)
					post[string(term)] = m
				}
				o := m[uint32(docid)]
				if o == nil {
					o = &brutePosting{}
					m[uint32(docid)] = o
				}
				o.tf++
				o.mask |= 1 << f
				o.pos[f] = append(o.pos[f], uint16(pos))
			})
		}
	}
	return post
}

func runPass(t *testing.T, dir string, rows []coldfmt.PageRow, budget int) *ShardOutput {
	t.Helper()
	p := NewShardPass(dir, budget)
	for i := range rows {
		if err := p.AddRow(&rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	out, err := p.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestShardPassMatchesBruteForce(t *testing.T) {
	rows := testRows(t)
	// A tiny budget forces multiple runs, so the merge-side aggregation
	// below crosses run boundaries.
	out := runPass(t, t.TempDir(), rows, 512)
	if len(out.Runs) < 2 {
		t.Fatalf("budget 512 produced %d runs, want several", len(out.Runs))
	}
	if out.DocCount != uint32(len(rows)) {
		t.Fatalf("doc count %d want %d", out.DocCount, len(rows))
	}

	// Runs must each be sorted by (term, docid) and every record must
	// match the brute-force posting for its (term, docid).
	want := brute(rows)
	seen := 0
	for _, path := range out.Runs {
		r, err := OpenRun(path)
		if err != nil {
			t.Fatal(err)
		}
		var prevTerm []byte
		prevDocid := int64(-1)
		var rec Rec
		for {
			err := r.Next(&rec)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if c := bytes.Compare(prevTerm, rec.Term); c > 0 || (c == 0 && int64(rec.Docid) <= prevDocid) {
				t.Fatalf("%s out of order at %q doc %d", path, rec.Term, rec.Docid)
			}
			prevTerm = append(prevTerm[:0], rec.Term...)
			prevDocid = int64(rec.Docid)

			bp := want[string(rec.Term)][rec.Docid]
			if bp == nil {
				t.Fatalf("unexpected posting %q doc %d", rec.Term, rec.Docid)
			}
			if rec.TF != bp.tf || rec.Mask != bp.mask {
				t.Fatalf("%q doc %d: tf %d mask %d, want tf %d mask %d", rec.Term, rec.Docid, rec.TF, rec.Mask, bp.tf, bp.mask)
			}
			for f := range NumFields {
				if len(rec.Pos[f]) != len(bp.pos[f]) {
					t.Fatalf("%q doc %d field %d positions %v want %v", rec.Term, rec.Docid, f, rec.Pos[f], bp.pos[f])
				}
				for j := range rec.Pos[f] {
					if rec.Pos[f][j] != bp.pos[f][j] {
						t.Fatalf("%q doc %d field %d positions %v want %v", rec.Term, rec.Docid, f, rec.Pos[f], bp.pos[f])
					}
				}
			}
			seen++
		}
		_ = r.Close()
	}
	total := 0
	for _, m := range want {
		total += len(m)
	}
	if seen != total {
		t.Fatalf("runs carry %d postings, brute force says %d", seen, total)
	}
}

func TestShardPassBands(t *testing.T) {
	rows := testRows(t)
	out := runPass(t, t.TempDir(), rows, 0)
	if len(out.Runs) != 1 {
		t.Fatalf("default budget produced %d runs", len(out.Runs))
	}

	var band bytes.Buffer
	if err := out.Docs.Seal(&band); err != nil {
		t.Fatal(err)
	}
	docs, err := hotfmt.OpenDocBand(band.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer docs.Close()
	rec, err := docs.Doc(2)
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.URL) != rows[2].URL || string(rec.Title) != rows[2].Title {
		t.Fatalf("doc band record 2: %s / %s", rec.URL, rec.Title)
	}
	if len(rec.Snippet) == 0 || len(rec.Snippet) > snippetBudget {
		t.Fatalf("snippet length %d", len(rec.Snippet))
	}

	if len(out.DocValues) != len(rows) {
		t.Fatalf("%d docvalues", len(out.DocValues))
	}
	if _, err := hotfmt.EncodeDocValues(out.DocValues); err != nil {
		t.Fatalf("docvalues do not encode: %v", err)
	}
	if out.DocValues[4].Lang != 1 || out.DocValues[0].DoclenBody == 0 {
		t.Fatalf("docvalues shape: %+v", out.DocValues[0])
	}
	if out.SumLen[fieldBody] == 0 || out.SumLen[fieldTitle] == 0 {
		t.Fatalf("field sums %v", out.SumLen)
	}
	if out.Stats[tokenize.Word] == 0 || out.Stats[tokenize.CJKBigram] == 0 {
		t.Fatalf("admission histogram empty: %v", out.Stats)
	}
}

func TestShardPassDeterministic(t *testing.T) {
	rows := testRows(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	a := runPass(t, dirA, rows, 512)
	b := runPass(t, dirB, rows, 512)
	if len(a.Runs) != len(b.Runs) {
		t.Fatalf("run counts differ: %d vs %d", len(a.Runs), len(b.Runs))
	}
	for i := range a.Runs {
		da, err := os.ReadFile(a.Runs[i])
		if err != nil {
			t.Fatal(err)
		}
		db, err := os.ReadFile(b.Runs[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(da, db) {
			t.Fatalf("run %d bytes differ across builds", i)
		}
	}
	var bandA, bandB bytes.Buffer
	if err := a.Docs.Seal(&bandA); err != nil {
		t.Fatal(err)
	}
	if err := b.Docs.Seal(&bandB); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bandA.Bytes(), bandB.Bytes()) {
		t.Fatal("doc band bytes differ across builds")
	}
}

func TestSnippetSource(t *testing.T) {
	short := "a short page."
	if got := snippetSource(short); string(got) != short {
		t.Fatalf("short text: %q", got)
	}
	long := strings.Repeat("filler words repeat here again and again. ", 40) +
		"unique dense sentence full of distinct informative vocabulary tokens. " +
		strings.Repeat("tail padding. ", 40)
	got := snippetSource(long)
	if len(got) > snippetBudget {
		t.Fatalf("snippet %d bytes over budget", len(got))
	}
	if len(got) == 0 {
		t.Fatal("empty snippet")
	}
	// No punctuation at all still yields a bounded passage.
	got = snippetSource(strings.Repeat("word ", 1000))
	if len(got) == 0 || len(got) > snippetBudget {
		t.Fatalf("punctuation-free snippet %d bytes", len(got))
	}
}
