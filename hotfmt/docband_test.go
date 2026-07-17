package hotfmt

import (
	"bytes"
	"fmt"
	"testing"
)

func fixtureDocRecord(i int) DocRecord {
	r := DocRecord{
		URL:     fmt.Appendf(nil, "https://example%03d.test/pages/article-%d", i%50, i),
		Title:   fmt.Appendf(nil, "Article %d about web search engines and indexing", i),
		Snippet: fmt.Appendf(nil, "This is the snippet source for document %d. It talks about crawling, indexing, and serving results quickly.", i),
	}
	for j := range r.URLFP {
		r.URLFP[j] = byte(i + j)
	}
	return r
}

func buildDocBand(n int) ([]byte, error) {
	var w DocBandWriter
	for i := range n {
		if err := w.Add(fixtureDocRecord(i)); err != nil {
			return nil, err
		}
	}
	return w.Seal()
}

func checkDocBand(t *testing.T, band []byte, n int) {
	t.Helper()
	b, err := OpenDocBand(band)
	if err != nil {
		t.Fatalf("OpenDocBand: %v", err)
	}
	defer b.Close()
	if b.NDocs() != uint32(n) {
		t.Fatalf("NDocs = %d, want %d", b.NDocs(), n)
	}
	for i := range n {
		got, err := b.Doc(uint32(i))
		if err != nil {
			t.Fatalf("Doc(%d): %v", i, err)
		}
		want := fixtureDocRecord(i)
		if !bytes.Equal(got.URL, want.URL) || !bytes.Equal(got.Title, want.Title) ||
			!bytes.Equal(got.Snippet, want.Snippet) || got.URLFP != want.URLFP {
			t.Fatalf("Doc(%d) drifted", i)
		}
	}
	if _, err := b.Doc(uint32(n)); err == nil {
		t.Fatal("docid past band accepted")
	}
}

func TestDocBandRoundTrip(t *testing.T) {
	// 200 docs: dictionary trains, 4 blocks with a partial last block.
	band, err := buildDocBand(200)
	if err != nil {
		t.Fatal(err)
	}
	checkDocBand(t, band, 200)
}

func TestDocBandNoDict(t *testing.T) {
	// Below the sample floor the band ships without a dictionary.
	band, err := buildDocBand(3)
	if err != nil {
		t.Fatal(err)
	}
	checkDocBand(t, band, 3)
}

func TestDocBandRejects(t *testing.T) {
	var w DocBandWriter
	if _, err := w.Seal(); err == nil {
		t.Error("empty band sealed")
	}
	if err := w.Add(DocRecord{}); err == nil {
		t.Error("empty url accepted")
	}
	if err := w.Add(DocRecord{URL: bytes.Repeat([]byte("u"), maxDocField+1)}); err == nil {
		t.Error("oversize url accepted")
	}

	band, err := buildDocBand(100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDocBand(band[:10]); err == nil {
		t.Error("truncated band accepted")
	}
	if _, err := OpenDocBand(band[1:]); err == nil {
		t.Error("shifted band accepted")
	}
	// A one-doc count skew keeps the block geometry, so Open cannot see
	// it without decompressing; it must surface when the block is read.
	bad := bytes.Clone(band)
	bad[len(bad)-4]++ // ndocs 100 -> 101
	if b, err := OpenDocBand(bad); err == nil {
		if _, derr := b.Doc(100); derr == nil {
			t.Error("doc count skew never surfaced")
		}
		b.Close()
	}
	bad = bytes.Clone(band)
	bad[len(bad)-4] = 0 // ndocs 0
	if _, err := OpenDocBand(bad); err == nil {
		t.Error("zero doc count accepted")
	}
}

func FuzzOpenDocBand(f *testing.F) {
	for _, n := range []int{3, 70} {
		if band, err := buildDocBand(n); err == nil {
			f.Add(band)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		b, err := OpenDocBand(data)
		if err != nil {
			return
		}
		defer b.Close()
		// Semantic invariant: every doc either reads cleanly or errors;
		// the ones that read survive a rebuild of the band.
		var w DocBandWriter
		for i := range b.NDocs() {
			r, err := b.Doc(i)
			if err != nil {
				return
			}
			if err := w.Add(r); err != nil {
				t.Fatalf("doc %d read from an accepted band but rejected by the writer: %v", i, err)
			}
		}
		band2, err := w.Seal()
		if err != nil {
			t.Fatalf("re-seal: %v", err)
		}
		b2, err := OpenDocBand(band2)
		if err != nil {
			t.Fatalf("reopen of rebuilt band: %v", err)
		}
		defer b2.Close()
		for i := range b.NDocs() {
			r1, err1 := b.Doc(i)
			r2, err2 := b2.Doc(i)
			if err1 != nil || err2 != nil {
				t.Fatalf("doc %d: %v / %v", i, err1, err2)
			}
			if !bytes.Equal(r1.URL, r2.URL) || !bytes.Equal(r1.Title, r2.Title) ||
				!bytes.Equal(r1.Snippet, r2.Snippet) || r1.URLFP != r2.URLFP {
				t.Fatalf("doc %d drifted through rebuild", i)
			}
		}
	})
}
