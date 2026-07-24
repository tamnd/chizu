package crawl

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/urlnorm"
)

// memSink collects committed segments for inspection.
type memSink struct {
	metas []SegMeta
	segs  [][]byte
}

func (s *memSink) Commit(_ context.Context, meta SegMeta, data []byte) error {
	s.metas = append(s.metas, meta)
	s.segs = append(s.segs, bytes.Clone(data))
	return nil
}

func wetRecord(uri, date, body string) string {
	return fmt.Sprintf("WARC/1.0\r\n"+
		"WARC-Type: conversion\r\n"+
		"WARC-Target-URI: %s\r\n"+
		"WARC-Date: %s\r\n"+
		"Content-Length: %d\r\n"+
		"\r\n%s\r\n\r\n", uri, date, len(body), body)
}

func gzWET(records ...string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	info := "WARC/1.0\r\nWARC-Type: warcinfo\r\nContent-Length: 4\r\n\r\ntest\r\n\r\n"
	for _, rec := range append([]string{info}, records...) {
		if _, err := zw.Write([]byte(rec)); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func body(seed string) string {
	return strings.Repeat("words about "+seed+" that fill a page nicely ", 4)
}

func TestImportWET(t *testing.T) {
	sink := &memSink{}
	im := &Importer{Parts: 1, Epoch: 7, Writer: 42, Sink: sink}
	wet := gzWET(
		wetRecord("HTTP://Example.COM:80/a/../b?utm_source=x&q=1", "2026-06-05T21:48:11Z", body("alpha")),
		wetRecord("http://example.com/b?q=1", "2026-06-06T00:00:00Z", body("alpha")), // same canon URL + same text: dup
		wetRecord("https://other.net/page", "2026-06-05T22:00:00Z", body("beta")),
		wetRecord("ftp://example.com/x", "2026-06-05T22:00:00Z", body("gamma")), // rejected by the laws
		wetRecord("https://example.com/tiny", "2026-06-05T22:00:00Z", "too short"),
	)
	ctx := context.Background()
	if err := im.ImportWET(ctx, bytes.NewReader(wet)); err != nil {
		t.Fatal(err)
	}
	if err := im.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if im.Stats.Pages != 2 || im.Stats.Dups != 1 || im.Stats.Rejected != 1 || im.Stats.Short != 1 || im.Stats.Segments != 1 {
		t.Fatalf("stats = %+v", im.Stats)
	}
	seg, err := coldfmt.OpenPageSegment(sink.segs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()
	rows, err := seg.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	r := rows[0]
	if r.URL != "http://example.com/b?q=1" {
		t.Errorf("URL = %q, not canonicalized", r.URL)
	}
	if r.URLFP != urlnorm.Fingerprint(r.URL) {
		t.Error("URLFP does not match the canonical fingerprint")
	}
	if r.Flags&coldfmt.PageImported == 0 {
		t.Error("provenance flag missing (CR-I6/doc 03 section 13)")
	}
	if r.LawVer != urlnorm.LawVersion {
		t.Errorf("LawVer = %d, want %d", r.LawVer, urlnorm.LawVersion)
	}
	if r.Simhash == 0 || r.SHA256 == [32]byte{} {
		t.Error("row is missing its hashes (CR-I3)")
	}
	if r.FetchMS != 1780696091000 { // 2026-06-05T21:48:11Z
		t.Errorf("FetchMS = %d, WARC-Date not preserved", r.FetchMS)
	}
	if !strings.Contains(r.Text, "alpha") {
		t.Error("text not preserved")
	}
	meta := sink.metas[0]
	if meta.Rows != 2 || meta.Seq != 1 || meta.Watermark != 1 || meta.Bytes != uint64(len(sink.segs[0])) {
		t.Errorf("meta = %+v", meta)
	}
}

func TestPartitionRouting(t *testing.T) {
	// The same host always lands in the same partition, port ignored.
	p := partition("https://example.com/x", 16)
	for _, u := range []string{
		"https://example.com/y?q=1",
		"https://example.com:8443/z",
		"http://example.com/",
	} {
		if got := partition(u, 16); got != p {
			t.Errorf("partition(%q) = %d, want %d", u, got, p)
		}
	}
	// Distinct hosts spread: with 16 partitions and 64 hosts, at least
	// two partitions must be hit (collision of all 64 would be a bug).
	hit := map[uint16]bool{}
	for i := range 64 {
		hit[partition(fmt.Sprintf("https://host%d.example/", i), 16)] = true
	}
	if len(hit) < 2 {
		t.Fatalf("64 hosts landed in %d partition(s)", len(hit))
	}
}

func TestSealAtRowsAndDedupAcrossSegments(t *testing.T) {
	sink := &memSink{}
	im := &Importer{Parts: 1, Epoch: 1, Writer: 1, SegRows: 2, Sink: sink}
	var recs []string
	for i := range 5 {
		recs = append(recs, wetRecord(fmt.Sprintf("https://h.example/p%d", i),
			"2026-06-05T21:48:11Z", body(fmt.Sprintf("page%d", i))))
	}
	// A duplicate of page 0, fed after the first segment sealed.
	recs = append(recs, wetRecord("https://h.example/p0", "2026-06-07T00:00:00Z", body("page0")))
	ctx := context.Background()
	if err := im.ImportWET(ctx, bytes.NewReader(gzWET(recs...))); err != nil {
		t.Fatal(err)
	}
	if err := im.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if im.Stats.Segments != 3 || im.Stats.Pages != 5 || im.Stats.Dups != 1 {
		t.Fatalf("stats = %+v", im.Stats)
	}
	for i, meta := range sink.metas {
		if meta.Seq != uint64(i+1) {
			t.Errorf("segment %d has seq %d, sequence not dense", i, meta.Seq)
		}
	}
}

func TestSameURLDifferentContentStored(t *testing.T) {
	sink := &memSink{}
	im := &Importer{Parts: 1, Sink: sink}
	ctx := context.Background()
	wet := gzWET(
		wetRecord("https://h.example/p", "2026-06-05T21:48:11Z", body("v1")),
		wetRecord("https://h.example/p", "2026-06-06T21:48:11Z", body("v2")),
	)
	if err := im.ImportWET(ctx, bytes.NewReader(wet)); err != nil {
		t.Fatal(err)
	}
	if im.Stats.Pages != 2 || im.Stats.Dups != 0 {
		t.Fatalf("stats = %+v: changed content must store, only (urlfp, hash) pairs fold", im.Stats)
	}
}

func FuzzReadWARC(f *testing.F) {
	f.Add([]byte("WARC/1.0\r\nWARC-Type: conversion\r\nContent-Length: 4\r\n\r\nbody\r\n\r\n"))
	f.Add([]byte("WARC/1.0\r\nContent-Length: 99999999999999999999\r\n\r\n"))
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic and never allocate past the payload cap.
		_ = readWARC(bufio.NewReader(bytes.NewReader(data)), func(h map[string]string, p []byte) error {
			if len(p) > maxWARCPayload {
				t.Fatalf("payload of %d bytes escaped the cap", len(p))
			}
			return nil
		})
	})
}
