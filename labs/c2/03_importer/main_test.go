package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wetPage builds one WET conversion record.
func wetPage(uri, body string) string {
	return fmt.Sprintf("WARC/1.0\r\n"+
		"WARC-Type: conversion\r\n"+
		"WARC-Target-URI: %s\r\n"+
		"Content-Length: %d\r\n"+
		"\r\n%s\r\n\r\n", uri, len(body), body)
}

// wetFixture is a small WET file: a warcinfo record the stages skip,
// three conversion records, one of them too short to store and one with
// a URL the laws reject.
func wetFixture() []byte {
	info := "WARC/1.0\r\nWARC-Type: warcinfo\r\nContent-Length: 4\r\n\r\ntest\r\n\r\n"
	long1 := wetPage("HTTP://Example.COM:80/a/../b?utm_source=x&q=1",
		strings.Repeat("the quick brown fox jumps over seven lazy dogs ", 4))
	long2 := wetPage("https://münchen.de/straße",
		strings.Repeat("pack my box with five dozen liquor jugs today ", 4))
	short := wetPage("https://example.com/tiny", "too short")
	bad := wetPage("ftp://example.com/file",
		strings.Repeat("this page has an unfetchable scheme and is skipped ", 4))
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for _, rec := range []string{info, long1, long2, short, bad} {
		if _, err := zw.Write([]byte(rec)); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestStages(t *testing.T) {
	files := [][]byte{wetFixture()}
	for _, st := range []struct {
		name string
		run  func([][]byte) (int, int64, error)
	}{
		{"parse", stageParse},
		{"canon", stageCanon},
		{"hash", stageHash},
		{"encode", stageEncode},
	} {
		pages, raw, err := st.run(files)
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		// The short record is dropped at parse; the ftp record parses as
		// a page but is skipped inside the later stages, so every stage
		// reports the same page count.
		if pages != 3 {
			t.Errorf("%s: pages = %d, want 3", st.name, pages)
		}
		if raw < 100 {
			t.Errorf("%s: raw = %d, implausibly small", st.name, raw)
		}
	}
}

func TestSimhash(t *testing.T) {
	a := simhash([]byte("the quick brown fox jumps over the lazy dog"))
	if a == 0 {
		t.Fatal("simhash of real text is zero")
	}
	if a != simhash([]byte("the  quick\nbrown fox jumps over the lazy dog")) {
		t.Error("simhash changed under whitespace-only edits")
	}
	b := simhash([]byte("completely different words about nothing similar here"))
	if a == b {
		t.Error("distinct texts share a simhash")
	}
	if simhash(nil) != 0 {
		t.Error("empty text should hash to zero votes everywhere")
	}
}

func TestFetch(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1<<16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	keep := t.TempDir()
	n, err := fetchOne(srv.URL+"/", "crawl-data/seg/a.wet.gz", keep)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Fatalf("fetched %d bytes, want %d", n, len(body))
	}
	got, err := os.ReadFile(filepath.Join(keep, "a.wet.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("kept file differs from served body")
	}
	if _, err := fetchOne(srv.URL+"/", "missing.wet.gz", ""); err == nil {
		t.Fatal("404 did not error")
	}
}

func TestReadPaths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wet.paths")
	if err := os.WriteFile(p, []byte("a\n\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := readPaths(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Fatalf("readPaths = %v, want [a b]", list)
	}
}
