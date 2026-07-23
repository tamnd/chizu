package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tamnd/chizu/coldfmt"
)

const wetFixture = "WARC/1.0\r\n" +
	"WARC-Type: warcinfo\r\n" +
	"Content-Length: 4\r\n" +
	"\r\n" +
	"info\r\n" +
	"\r\n" +
	"WARC/1.0\r\n" +
	"WARC-Type: conversion\r\n" +
	"WARC-Target-URI: https://example.com/a\r\n" +
	"Content-Length: 100\r\n" +
	"\r\n" +
	"0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789\r\n" +
	"\r\n" +
	"WARC/1.0\r\n" +
	"WARC-Type: conversion\r\n" +
	"Content-Length: 5\r\n" +
	"\r\n" +
	"tiny.\r\n" +
	"\r\n"

func TestWETParse(t *testing.T) {
	var got [][]byte
	err := readWARC(bufio.NewReader(strings.NewReader(wetFixture)), func(h map[string]string, payload []byte) error {
		if rec := extractText(h, payload); rec != nil {
			got = append(got, rec)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// warcinfo and the 5-byte record fall out; the 100-byte one stays.
	if len(got) != 1 || len(got[0]) != 100 {
		t.Fatalf("got %d records", len(got))
	}
}

const watPayload = `{"Envelope":{"Payload-Metadata":{"HTTP-Response-Metadata":{"HTML-Metadata":{
  "Links":[
    {"path":"A@/href","url":"https://example.com/x","text":"an anchor"},
    {"path":"IMG@/src","url":"https://example.com/pic.png"},
    {"path":"A@/href","url":"https://example.com/y","text":"paid","rel":"sponsored nofollow"}
  ]}}}}}`

func TestWATLinks(t *testing.T) {
	h := map[string]string{"warc-type": "metadata"}
	row := extractLinks(h, []byte(watPayload))
	if row == nil {
		t.Fatal("no row")
	}
	count, n := binary.Uvarint(row)
	if count != 2 {
		t.Fatalf("count %d, want 2 (IMG link filtered)", count)
	}
	// First link: 16B fp, flags 0, anchor "an anchor".
	p := row[n:]
	p = p[16:]
	if p[0] != 0 {
		t.Fatalf("first link flags %b", p[0])
	}
	alen, n2 := binary.Uvarint(p[1:])
	if string(p[1+n2:1+n2+int(alen)]) != "an anchor" {
		t.Fatalf("anchor %q", p[1+n2:1+n2+int(alen)])
	}
	// Second link carries nofollow and sponsored bits.
	p = p[1+n2+int(alen):]
	p = p[16:]
	want := byte(coldfmt.LinkNofollow) | byte(coldfmt.LinkSponsored)
	if p[0] != want {
		t.Fatalf("second link flags %b, want %b", p[0], want)
	}
	if extractLinks(map[string]string{"warc-type": "conversion"}, []byte(watPayload)) != nil {
		t.Fatal("non-metadata record produced a row")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.rec")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	recs := [][]byte{[]byte("alpha"), []byte(""), bytes.Repeat([]byte("z"), 5000)}
	for _, r := range recs {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(r)))
		if _, err := f.Write(hdr[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(recs) {
		t.Fatalf("%d records", len(got))
	}
	for i := range recs {
		if !bytes.Equal(got[i], recs[i]) {
			t.Fatalf("record %d mismatch", i)
		}
	}
}

// TestArmsSmoke runs every dictionary/level arm over a small synthetic
// corpus with a tiny block target so multi-block paths execute. Ratios
// and rates carry no perf claim; only round-trip and shape are pinned.
func TestArmsSmoke(t *testing.T) {
	var text [][]byte
	for i := range 200 {
		body := fmt.Sprintf("page %d: the quick brown fox jumps over lazy dog number %d. ", i, i*7919)
		text = append(text, bytes.Repeat([]byte(body), 10+i%7))
	}
	blocks := buildBlocks(text, true, 8<<10)
	if len(blocks) < 2 {
		t.Fatalf("%d blocks, want multi-block", len(blocks))
	}
	trained := trainDict(text)
	raw := rawDict(text)
	if raw == nil || len(raw) > coldfmt.DictSize {
		t.Fatalf("raw dict %d bytes", len(raw))
	}
	arms := []struct {
		kind string
		d    []byte
	}{{"none", nil}, {"raw", raw}}
	if trained != nil {
		arms = append(arms, struct {
			kind string
			d    []byte
		}{"trained", trained})
	}
	for _, a := range arms {
		for _, lvl := range []int{1, 3} {
			r, err := runArm(blocks, a.kind, a.d, lvl, 1)
			if err != nil {
				t.Fatalf("%s/%d: %v", a.kind, lvl, err)
			}
			if r.stored <= 0 || r.stored >= r.raw {
				t.Fatalf("%s/%d: stored %d of raw %d, repeated text must compress", a.kind, lvl, r.stored, r.raw)
			}
		}
	}
}
