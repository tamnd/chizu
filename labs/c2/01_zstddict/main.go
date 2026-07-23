// Lab zstd-dict (doc 04 section 14): compression ratio and decode rate
// for the text and outlinks columns with and without per-segment
// dictionaries at levels 1/3, on real Common Crawl data. Gates the
// comp-2 dictionary default and the ~3 KiB/page stored-text constant.
//
// Two modes:
//
//	prep  -wet a.wet.gz,b.wet.gz -out text.rec
//	prep  -wat a.wat.gz,b.wat.gz -out links.rec
//	sweep -text text.rec -links links.rec -label server3 -reps 3
//
// prep turns WET conversion records into text records and WAT metadata
// records into doc 04 section 3.1 outlink rows; sweep builds blocks the
// way coldfmt does (len-prefixed cells to BlockRawTarget raw) and runs
// the dictionary/level arms. Both dictionaries train on text and serve
// both columns, exactly like coldfmt's per-segment trainDict.
//
// Output is TSV: label col dict level blocks pages rawMB storedMB
// ratio bPerPage encMBps decMBps trainMS.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"
	"github.com/tamnd/chizu/coldfmt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: zstddict prep|sweep [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "prep":
		err = prepCmd(os.Args[2:])
	case "sweep":
		err = sweepCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zstddict:", err)
		os.Exit(1)
	}
}

// ---- prep ----

func prepCmd(args []string) error {
	fs := flag.NewFlagSet("prep", flag.ContinueOnError)
	wet := fs.String("wet", "", "comma list of .wet.gz files")
	wat := fs.String("wat", "", "comma list of .wat.gz files")
	out := fs.String("out", "", "record file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || (*wet == "") == (*wat == "") {
		return errors.New("prep needs -out and exactly one of -wet/-wat")
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriterSize(f, 1<<20)
	n := 0
	emit := func(rec []byte) error {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(rec)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		_, err := w.Write(rec)
		n++
		return err
	}
	paths := *wet
	extract := extractText
	if *wat != "" {
		paths = *wat
		extract = extractLinks
	}
	for _, path := range strings.Split(paths, ",") {
		if err := prepFile(path, extract, emit); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prep: %d records -> %s\n", n, *out)
	return f.Close()
}

func prepFile(path string, extract func(map[string]string, []byte) []byte, emit func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	return readWARC(bufio.NewReaderSize(gz, 1<<20), func(h map[string]string, payload []byte) error {
		if rec := extract(h, payload); rec != nil {
			return emit(rec)
		}
		return nil
	})
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

// extractText keeps WET conversion records with enough body to matter.
func extractText(h map[string]string, payload []byte) []byte {
	if h["warc-type"] != "conversion" || len(payload) < 64 {
		return nil
	}
	return bytes.Clone(payload)
}

// watEnvelope is the slice of the WAT JSON the lab needs.
type watEnvelope struct {
	Envelope struct {
		PayloadMetadata struct {
			HTTPResponseMetadata struct {
				HTMLMetadata struct {
					Links []watLink `json:"Links"`
				} `json:"HTML-Metadata"`
			} `json:"HTTP-Response-Metadata"`
		} `json:"Payload-Metadata"`
	} `json:"Envelope"`
}

type watLink struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Text string `json:"text"`
	Rel  string `json:"rel"`
}

// extractLinks turns a WAT metadata record into a doc 04 section 3.1
// outlink row: count, then per link urlfp 16B, flags, anchor.
func extractLinks(h map[string]string, payload []byte) []byte {
	if h["warc-type"] != "metadata" {
		return nil
	}
	var env watEnvelope
	if json.Unmarshal(payload, &env) != nil {
		return nil
	}
	var links []watLink
	for _, l := range env.Envelope.PayloadMetadata.HTTPResponseMetadata.HTMLMetadata.Links {
		if l.Path == "A@/href" && l.URL != "" {
			links = append(links, l)
		}
	}
	if len(links) == 0 {
		return nil
	}
	row := coldfmt.AppendUvarint(nil, uint64(len(links)))
	for _, l := range links {
		fp := sha256.Sum256([]byte(l.URL))
		row = append(row, fp[:16]...)
		var flags byte
		if strings.Contains(l.Rel, "nofollow") {
			flags |= byte(coldfmt.LinkNofollow)
		}
		if strings.Contains(l.Rel, "ugc") {
			flags |= byte(coldfmt.LinkUGC)
		}
		if strings.Contains(l.Rel, "sponsored") {
			flags |= byte(coldfmt.LinkSponsored)
		}
		row = append(row, flags)
		anchor := l.Text
		if len(anchor) > coldfmt.MaxAnchorLen {
			anchor = anchor[:coldfmt.MaxAnchorLen]
		}
		row = coldfmt.AppendUvarint(row, uint64(len(anchor)))
		row = append(row, anchor...)
	}
	return row
}

// ---- sweep ----

func sweepCmd(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	textPath := fs.String("text", "", "text record file from prep -wet")
	linksPath := fs.String("links", "", "outlinks record file from prep -wat")
	label := fs.String("label", "local", "row label, name the host")
	reps := fs.Int("reps", 3, "decode passes per arm")
	target := fs.Int("target", coldfmt.BlockRawTarget, "raw bytes per block")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *textPath == "" {
		return errors.New("sweep needs -text (dictionaries train on the text column)")
	}
	text, err := readRecords(*textPath)
	if err != nil {
		return err
	}
	var links [][]byte
	if *linksPath != "" {
		if links, err = readRecords(*linksPath); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "sweep: %d text rows, %d link rows\n", len(text), len(links))

	trainStart := time.Now()
	trained := trainDict(text)
	trainMS := time.Since(trainStart).Milliseconds()
	raw := rawDict(text)
	if trained == nil {
		fmt.Fprintln(os.Stderr, "sweep: dict training failed, trained arm skipped")
	}

	fmt.Println("label\tcol\tdict\tlevel\tblocks\tpages\trawMB\tstoredMB\tratio\tbPerPage\tencMBps\tdecMBps\ttrainMS")
	cols := []struct {
		name string
		rows [][]byte
		text bool
	}{{"text", text, true}, {"outlinks", links, false}}
	for _, col := range cols {
		if len(col.rows) == 0 {
			continue
		}
		blocks := buildBlocks(col.rows, col.text, *target)
		for _, d := range []struct {
			name  string
			bytes []byte
		}{{"none", nil}, {"trained", trained}, {"raw", raw}} {
			if d.name != "none" && d.bytes == nil {
				continue
			}
			for _, lvl := range []int{1, 3} {
				r, err := runArm(blocks, d.name, d.bytes, lvl, *reps)
				if err != nil {
					return fmt.Errorf("%s/%s/%d: %w", col.name, d.name, lvl, err)
				}
				tm := int64(0)
				if d.name == "trained" {
					tm = trainMS
				}
				fmt.Printf("%s\t%s\t%s\t%d\t%d\t%d\t%.1f\t%.1f\t%.4f\t%.0f\t%.0f\t%.0f\t%d\n",
					*label, col.name, d.name, lvl, len(blocks), len(col.rows),
					mb(r.raw), mb(r.stored), float64(r.stored)/float64(r.raw),
					float64(r.stored)/float64(len(col.rows)), r.encMBps, r.decMBps, tm)
			}
		}
	}
	return nil
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }

func readRecords(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	var recs [][]byte
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err == io.EOF {
			return recs, nil
		} else if err != nil {
			return nil, err
		}
		rec := make([]byte, binary.LittleEndian.Uint32(hdr[:]))
		if _, err := io.ReadFull(r, rec); err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
}

// buildBlocks concatenates cells to the raw target the way coldfmt's
// Seal does: text cells are len-prefixed strings, outlink rows are
// already the section 3.1 encoding.
func buildBlocks(rows [][]byte, lenPrefix bool, target int) [][]byte {
	var blocks [][]byte
	var buf []byte
	for _, rec := range rows {
		if lenPrefix {
			buf = coldfmt.AppendUvarint(buf, uint64(len(rec)))
		}
		buf = append(buf, rec...)
		if len(buf) >= target {
			blocks = append(blocks, buf)
			buf = nil
		}
	}
	if len(buf) > 0 {
		blocks = append(blocks, buf)
	}
	return blocks
}

// trainDict mirrors coldfmt's per-segment training: first 1024 text
// rows, 64 KiB, the same builder parameters. The builder panics on
// degenerate corpora (all-identical samples), so that is caught too and
// the arm is skipped like any other training failure.
func trainDict(text [][]byte) (d []byte) {
	defer func() {
		if recover() != nil {
			d = nil
		}
	}()
	return trainDictInner(text)
}

func trainDictInner(text [][]byte) []byte {
	samples := text
	if len(samples) > 1024 {
		samples = samples[:1024]
	}
	if len(samples) < 8 {
		return nil
	}
	d, err := dict.BuildZstdDict(samples, dict.Options{
		MaxDictSize: coldfmt.DictSize,
		HashBytes:   6,
		ZstdDictID:  0x7A04,
	})
	if err != nil {
		return nil
	}
	return d
}

const rawDictID = 0x7A05

// rawDict stride-samples 64 KiB of content, the hotfmt doc-band
// mechanism: no entropy tables, just referenceable bytes.
func rawDict(text [][]byte) []byte {
	if len(text) == 0 {
		return nil
	}
	const chunk = 2 << 10
	want := coldfmt.DictSize / chunk
	stride := max(len(text)/want, 1)
	var d []byte
	for i := 0; i < len(text) && len(d) < coldfmt.DictSize; i += stride {
		c := text[i]
		if len(c) > chunk {
			c = c[:chunk]
		}
		d = append(d, c...)
	}
	if len(d) > coldfmt.DictSize {
		d = d[:coldfmt.DictSize]
	}
	return d
}

type armResult struct {
	raw, stored      int64
	encMBps, decMBps float64
}

func runArm(blocks [][]byte, dictKind string, dictBytes []byte, level, reps int) (armResult, error) {
	var res armResult
	encOpts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1),
	}
	decOpts := []zstd.DOption{zstd.WithDecoderConcurrency(1)}
	switch dictKind {
	case "trained":
		encOpts = append(encOpts, zstd.WithEncoderDict(dictBytes))
		decOpts = append(decOpts, zstd.WithDecoderDicts(dictBytes))
	case "raw":
		encOpts = append(encOpts, zstd.WithEncoderDictRaw(rawDictID, dictBytes))
		decOpts = append(decOpts, zstd.WithDecoderDictRaw(rawDictID, dictBytes))
	}
	enc, err := zstd.NewWriter(nil, encOpts...)
	if err != nil {
		return res, err
	}
	defer func() { _ = enc.Close() }()
	dec, err := zstd.NewReader(nil, decOpts...)
	if err != nil {
		return res, err
	}
	defer dec.Close()

	stored := make([][]byte, len(blocks))
	t0 := time.Now()
	for i, b := range blocks {
		stored[i] = enc.EncodeAll(b, nil)
		res.raw += int64(len(b))
		res.stored += int64(len(stored[i]))
	}
	res.encMBps = mb(res.raw) / time.Since(t0).Seconds()

	for i, s := range stored {
		got, err := dec.DecodeAll(s, nil)
		if err != nil {
			return res, err
		}
		if !bytes.Equal(got, blocks[i]) {
			return res, fmt.Errorf("block %d round-trip mismatch", i)
		}
	}
	t1 := time.Now()
	for range reps {
		for _, s := range stored {
			if _, err := dec.DecodeAll(s, nil); err != nil {
				return res, err
			}
		}
	}
	res.decMBps = mb(res.raw) * float64(reps) / time.Since(t1).Seconds()
	return res, nil
}
