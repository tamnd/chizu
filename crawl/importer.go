// Package crawl is the crawl plane. Its first resident is the Common
// Crawl importer (doc 03 section 13): the real bootstrap, which reads
// WET files from the public bucket and runs every row through the same
// canonicalize/dedup/store path a live fetch takes, law version stamped
// and provenance flagged imported. It runs as a synthetic crawl
// partition set: host hash picks the partition, each partition keeps
// its own dedup state and dense segment sequence, and every sealed
// segment is committed through the Sink (upload plus SegCommit), so
// nothing downstream can tell an imported partition from a crawled one.
package crawl

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/coldfmt"
	"github.com/tamnd/chizu/urlnorm"
)

// minTextLen drops WET records too short to ever rank; the row count in
// Stats.Short keeps the drop visible to corpus-stats.
const minTextLen = 64

// maxWARCPayload caps a declared Content-Length so a corrupt or hostile
// record cannot demand an arbitrary allocation (same discipline as
// coldfmt's maxSegmentRows).
const maxWARCPayload = 64 << 20

// Stats counts what the importer did; corpus-stats reads these numbers
// against the doc 00 tables.
type Stats struct {
	Pages    uint64 // rows stored
	Dups     uint64 // (urlfp, sha256) pairs folded, CR-I4's exactly-once
	Rejected uint64 // URLs unfetchable under the laws (never stored, CR-I5)
	Short    uint64 // conversion records under minTextLen
	Segments uint64 // segments committed
}

// SegMeta is what the Sink needs to upload and commit one sealed
// segment; the fields mirror chain.SegCommit.
type SegMeta struct {
	Family    byte
	Partition uint16
	Seq       uint64
	Rows      uint64
	Bytes     uint64
	Watermark uint64
}

// Sink lands a sealed segment: upload the bytes, then make them live
// with a SegCommit on the chain. An uploaded object without its commit
// record does not exist, so Commit must not return before both.
type Sink interface {
	Commit(ctx context.Context, meta SegMeta, data []byte) error
}

// Importer streams WET files into cold page segments across a synthetic
// partition set. Not safe for concurrent use: one importer is one
// writer, matching the chain's one-append-loop-per-node rule.
type Importer struct {
	Parts    uint16 // partition count of the synthetic set
	Epoch    uint32 // writer epoch stamped into every segment
	Writer   uint64 // writer id stamped into every segment
	SegRows  int    // seal threshold in rows (default 16384)
	SegBytes int64  // seal threshold in raw text bytes (default 64 MiB, doc 03 section 10)
	Sink     Sink

	Stats Stats
	parts []*partState
}

type partState struct {
	n    uint16
	w    coldfmt.PageSegmentWriter
	seen map[[48]byte]struct{} // urlfp ++ sha256, partition-local exact dedup
	seq  uint64
	raw  int64
}

func (im *Importer) init() {
	if im.parts != nil {
		return
	}
	if im.Parts == 0 {
		im.Parts = 1
	}
	if im.SegRows == 0 {
		im.SegRows = 16 << 10
	}
	if im.SegBytes == 0 {
		im.SegBytes = 64 << 20
	}
	im.parts = make([]*partState, im.Parts)
	for i := range im.parts {
		im.parts[i] = &partState{n: uint16(i), seen: make(map[[48]byte]struct{})}
	}
}

// ImportWET streams one .wet.gz file (multistream gzip) through the
// canonicalize/dedup/store path. Call Flush after the last file.
func (im *Importer) ImportWET(ctx context.Context, r io.Reader) error {
	im.init()
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	return readWARC(bufio.NewReaderSize(zr, 1<<20), func(h map[string]string, payload []byte) error {
		if h["warc-type"] != "conversion" {
			return nil
		}
		if len(payload) < minTextLen {
			im.Stats.Short++
			return nil
		}
		return im.store(ctx, h, payload)
	})
}

func (im *Importer) store(ctx context.Context, h map[string]string, text []byte) error {
	canon, err := urlnorm.Canonicalize(h["warc-target-uri"])
	if err != nil {
		im.Stats.Rejected++
		return nil
	}
	fp := urlnorm.Fingerprint(canon)
	sum := sha256.Sum256(text)
	part := im.parts[partition(canon, im.Parts)]

	var pair [48]byte
	copy(pair[:16], fp[:])
	copy(pair[16:], sum[:])
	if _, dup := part.seen[pair]; dup {
		// A repeated (urlfp, hash) pair is a refresh observation, not a
		// new row (CR-I4); the corpus sees exactly-once.
		im.Stats.Dups++
		return nil
	}
	part.seen[pair] = struct{}{}

	part.w.Add(coldfmt.PageRow{
		URLFP:   fp,
		URL:     canon,
		FetchMS: fetchMS(h["warc-date"]),
		Status:  200,
		Flags:   coldfmt.PageImported,
		SHA256:  sum,
		Simhash: simhash(text),
		Text:    string(text),
		LawVer:  urlnorm.LawVersion,
	})
	part.raw += int64(len(text) + len(canon) + 80)
	im.Stats.Pages++
	if part.w.NRows() >= im.SegRows || part.raw >= im.SegBytes {
		return im.seal(ctx, part)
	}
	return nil
}

// Flush seals every partition's open segment. The importer is reusable
// afterward; dedup state persists so a re-fed duplicate still folds.
func (im *Importer) Flush(ctx context.Context) error {
	im.init()
	for _, part := range im.parts {
		if part.w.NRows() == 0 {
			continue
		}
		if err := im.seal(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

func (im *Importer) seal(ctx context.Context, part *partState) error {
	part.seq++
	part.w.Partition = part.n
	part.w.Epoch = im.Epoch
	part.w.Seq = part.seq
	part.w.Writer = im.Writer
	rows := uint64(part.w.NRows())
	data, err := part.w.Seal()
	if err != nil {
		return err
	}
	meta := SegMeta{
		Family:    chain.FamilyPage,
		Partition: part.n,
		Seq:       part.seq,
		Rows:      rows,
		Bytes:     uint64(len(data)),
		Watermark: part.seq,
	}
	if err := im.Sink.Commit(ctx, meta, data); err != nil {
		return err
	}
	im.Stats.Segments++
	part.w = coldfmt.PageSegmentWriter{}
	part.raw = 0
	return nil
}

// partition routes a canonical URL to its partition by host hash
// (doc 03 section 2: a host lives in exactly one partition). The port
// is excluded so example.com:8080 shares its host's politeness home.
func partition(canon string, parts uint16) uint16 {
	host := canon[strings.Index(canon, "://")+3:]
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.HasSuffix(host, "]") {
		if _, err := strconv.Atoi(host[i+1:]); err == nil {
			host = host[:i]
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	return uint16(h.Sum32() % uint32(parts))
}

// fetchMS keeps the original crawl timestamp from WARC-Date; imported
// rows enter the refresh model with their real age, not import time.
func fetchMS(date string) uint64 {
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return 0
	}
	return uint64(t.UnixMilli())
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
			return fmt.Errorf("crawl: expected WARC version line, got %q", line)
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
		if err != nil || clen < 0 || clen > maxWARCPayload {
			return fmt.Errorf("crawl: bad content-length %q", h["content-length"])
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
