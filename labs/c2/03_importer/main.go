// Lab importer (doc 03 section 13, doc 10 CR4): import throughput and
// the inputs to the $/billion line. Gates the bootstrap schedule.
//
// Two modes:
//
//	fetch     -paths wet.paths -n 8 -conc 4 -label server3 [-keep dir]
//	transform -wet a.wet.gz,b.wet.gz -reps 3 -label server3
//
// fetch pulls WET files from the public bucket over HTTPS and measures
// aggregate bytes/s, nothing else: it answers whether the network can
// feed CR4's 5k pages/s/node. transform measures the per-core CPU
// decomposition of the import path as cumulative stages, each stage a
// full re-run from in-memory .gz bytes so disk never pollutes a delta:
//
//	parse   gunzip + WARC walk of conversion records
//	canon   + urlnorm.Canonicalize(WARC-Target-URI) + Fingerprint
//	hash    + sha256(text) + 64-bit token simhash
//	encode  + coldfmt page rows, Add + Seal (dict training and zstd in)
//
// held against the doc 03 CPU budget table (~150/50/30/15 us lines).
//
// Output is TSV. fetch: label arm conc files MB wallS MBps. transform:
// label stage files pages rawMB wallS usPerPage pagesPerS.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/urlnorm"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: importer fetch|transform [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = fetchCmd(os.Args[2:])
	case "transform":
		err = transformCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "importer:", err)
		os.Exit(1)
	}
}

// ---- fetch ----

const bucketBase = "https://data.commoncrawl.org/"

func fetchCmd(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	paths := fs.String("paths", "", "file of bucket-relative WET paths, one per line")
	n := fs.Int("n", 4, "files to fetch (first n lines)")
	conc := fs.Int("conc", 4, "concurrent connections")
	label := fs.String("label", "local", "row label, name the host")
	keep := fs.String("keep", "", "directory to keep downloaded files in")
	base := fs.String("base", bucketBase, "bucket base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *paths == "" {
		return errors.New("fetch needs -paths")
	}
	list, err := readPaths(*paths, *n)
	if err != nil {
		return err
	}
	if *keep != "" {
		if err := os.MkdirAll(*keep, 0o755); err != nil {
			return err
		}
	}

	var total atomic.Int64
	jobs := make(chan string)
	var wg sync.WaitGroup
	errCh := make(chan error, len(list))
	t0 := time.Now()
	for range *conc {
		wg.Go(func() {
			for p := range jobs {
				n, err := fetchOne(*base, p, *keep)
				if err != nil {
					errCh <- fmt.Errorf("%s: %w", p, err)
					return
				}
				total.Add(n)
			}
		})
	}
	for _, p := range list {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	wall := time.Since(t0).Seconds()
	select {
	case err := <-errCh:
		return err
	default:
	}
	mbs := float64(total.Load()) / (1 << 20)
	fmt.Println("label\tarm\tconc\tfiles\tMB\twallS\tMBps")
	fmt.Printf("%s\tfetch\t%d\t%d\t%.1f\t%.1f\t%.1f\n",
		*label, *conc, len(list), mbs, wall, mbs/wall)
	return nil
}

func readPaths(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var list []string
	sc := bufio.NewScanner(f)
	for sc.Scan() && len(list) < n {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			list = append(list, p)
		}
	}
	return list, sc.Err()
}

func fetchOne(base, path, keep string) (int64, error) {
	resp, err := http.Get(base + path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %s", resp.Status)
	}
	w := io.Discard
	if keep != "" {
		f, err := os.Create(filepath.Join(keep, filepath.Base(path)))
		if err != nil {
			return 0, err
		}
		defer func() { _ = f.Close() }()
		w = bufio.NewWriterSize(f, 1<<20)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, err
	}
	if bw, ok := w.(*bufio.Writer); ok {
		err = bw.Flush()
	}
	return n, err
}

// ---- transform ----

// page is what the parse stage yields and later stages consume.
type page struct {
	uri  string
	text []byte
}

func transformCmd(args []string) error {
	fs := flag.NewFlagSet("transform", flag.ContinueOnError)
	wet := fs.String("wet", "", "comma list of local .wet.gz files")
	reps := fs.Int("reps", 3, "timed passes per stage")
	label := fs.String("label", "local", "row label, name the host")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *wet == "" {
		return errors.New("transform needs -wet")
	}
	var files [][]byte
	for p := range strings.SplitSeq(*wet, ",") {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, b)
	}

	stages := []struct {
		name string
		run  func([][]byte) (int, int64, error)
	}{
		{"parse", stageParse},
		{"canon", stageCanon},
		{"hash", stageHash},
		{"encode", stageEncode},
	}
	fmt.Println("label\tstage\tfiles\tpages\trawMB\twallS\tusPerPage\tpagesPerS")
	for _, st := range stages {
		// One warm pass, then reps timed passes; each pass runs the full
		// cumulative pipeline from the in-memory .gz bytes.
		pages, raw, err := st.run(files)
		if err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
		t0 := time.Now()
		for range *reps {
			if _, _, err := st.run(files); err != nil {
				return fmt.Errorf("%s: %w", st.name, err)
			}
		}
		wall := time.Since(t0).Seconds() / float64(*reps)
		fmt.Printf("%s\t%s\t%d\t%d\t%.1f\t%.2f\t%.1f\t%.0f\n",
			*label, st.name, len(files), pages, float64(raw)/(1<<20),
			wall, wall*1e6/float64(pages), float64(pages)/wall)
	}
	return nil
}

// walkPages runs the WARC walk over one in-memory .wet.gz and calls fn
// for every conversion record worth storing.
func walkPages(gz []byte, fn func(page) error) (int, int64, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return 0, 0, err
	}
	pages, raw := 0, int64(0)
	err = readWARC(bufio.NewReaderSize(zr, 1<<20), func(h map[string]string, payload []byte) error {
		if h["warc-type"] != "conversion" || len(payload) < 64 {
			return nil
		}
		pages++
		raw += int64(len(payload))
		return fn(page{uri: h["warc-target-uri"], text: payload})
	})
	return pages, raw, err
}

func stageParse(files [][]byte) (int, int64, error) {
	pages, raw := 0, int64(0)
	for _, gz := range files {
		p, r, err := walkPages(gz, func(page) error { return nil })
		if err != nil {
			return 0, 0, err
		}
		pages += p
		raw += r
	}
	return pages, raw, nil
}

func stageCanon(files [][]byte) (int, int64, error) {
	pages, raw := 0, int64(0)
	var sink [16]byte
	for _, gz := range files {
		p, r, err := walkPages(gz, func(pg page) error {
			canon, err := urlnorm.Canonicalize(pg.uri)
			if err != nil {
				return nil // unfetchable under the laws: skipped, not fatal
			}
			sink = urlnorm.Fingerprint(canon)
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
		pages += p
		raw += r
	}
	_ = sink
	return pages, raw, nil
}

func stageHash(files [][]byte) (int, int64, error) {
	pages, raw := 0, int64(0)
	var sink uint64
	for _, gz := range files {
		p, r, err := walkPages(gz, func(pg page) error {
			canon, err := urlnorm.Canonicalize(pg.uri)
			if err != nil {
				return nil
			}
			_ = urlnorm.Fingerprint(canon)
			_ = sha256.Sum256(pg.text)
			sink = simhash(pg.text)
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
		pages += p
		raw += r
	}
	_ = sink
	return pages, raw, nil
}

// segmentRows matches what honest page segments hold (~16-20k rows);
// encode seals a segment every time it fills, like the importer will.
const segmentRows = 16 << 10

func stageEncode(files [][]byte) (int, int64, error) {
	pages, raw := 0, int64(0)
	var w coldfmt.PageSegmentWriter
	seal := func() error {
		if w.NRows() == 0 {
			return nil
		}
		_, err := w.Seal()
		w = coldfmt.PageSegmentWriter{}
		return err
	}
	for _, gz := range files {
		p, r, err := walkPages(gz, func(pg page) error {
			canon, err := urlnorm.Canonicalize(pg.uri)
			if err != nil {
				return nil
			}
			row := coldfmt.PageRow{
				URLFP:   urlnorm.Fingerprint(canon),
				URL:     canon,
				Status:  200,
				Flags:   coldfmt.PageImported,
				SHA256:  sha256.Sum256(pg.text),
				Simhash: simhash(pg.text),
				Text:    string(pg.text),
				LawVer:  urlnorm.LawVersion,
			}
			w.Add(row)
			if w.NRows() >= segmentRows {
				return seal()
			}
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
		pages += p
		raw += r
	}
	return pages, raw, seal()
}

// simhash is the 64-bit Charikar sketch over whitespace tokens with
// FNV-1a features, the same shape the doc 06 near-dup pass computes.
func simhash(text []byte) uint64 {
	const offset64, prime64 = 14695981039346656037, 1099511628211
	var acc [64]int32
	for tok := range bytes.FieldsSeq(text) {
		f := uint64(offset64)
		for _, c := range tok {
			f = (f ^ uint64(c)) * prime64
		}
		for b := range 64 {
			// Branchless vote: bit set adds +1, clear adds -1.
			acc[b] += int32(f>>b&1)*2 - 1
		}
	}
	var out uint64
	for b := range 64 {
		if acc[b] > 0 {
			out |= 1 << b
		}
	}
	return out
}

// readWARC walks WARC/1.0 records: header lines to a blank line, then
// Content-Length payload bytes, then a blank separator.
func readWARC(r *bufio.Reader, fn func(map[string]string, []byte) error) error {
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "WARC/") {
			return fmt.Errorf("expected WARC version line, got %q", line)
		}
		h := map[string]string{}
		for {
			line, err = r.ReadString('\n')
			if err != nil {
				return err
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok {
				h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
		clen, err := strconv.Atoi(h["content-length"])
		if err != nil {
			return fmt.Errorf("bad content-length: %w", err)
		}
		payload := make([]byte, clen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		if err := fn(h, payload); err != nil {
			return err
		}
	}
}
