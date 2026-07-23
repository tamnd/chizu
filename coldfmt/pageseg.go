package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/dict"
)

// Page segments, doc 04 section 3: one row group of pages in
// fetch-completion order, stored as columns so build stages read only what
// they need. Layout: common header, column blocks back to back, the shared
// zstd dictionary, the footer, the tail.

// Page flag bits, column 4. The bit table lives here per doc 04 section 3.
const (
	PageTruncated uint32 = 1 << iota
	PageRedirected
	PageAlias
	PageNearDup
	PageRawStored
	PageImported
)

// Outlink flag bits, doc 04 section 3.1.
const (
	LinkNofollow byte = 1 << iota
	LinkUGC
	LinkSponsored
	LinkCanonAlias
)

// MaxAnchorLen is where anchors truncate at store time (doc 04 section 3.1).
const MaxAnchorLen = 256

// BlockRawTarget is how much raw column data goes in one block before it is
// compressed toward the 8-16 MiB stored target. Provisional until the C1
// lab cold-block-size sweeps it on real reads.
const BlockRawTarget = 16 << 20

// DictSize is the shared per-segment dictionary size (doc 04 section 3).
const DictSize = 64 << 10

// maxSegmentRows caps a declared row count so a corrupt footer cannot
// demand an arbitrary allocation; honest segments hold ~16-20k rows.
const maxSegmentRows = 1 << 24

// Outlink is one destination in the outlinks column. Dst is a urlfp, not a
// node id, because node ids are per-graph-generation artifacts and page
// rows must outlive graph generations.
type Outlink struct {
	Dst    [16]byte
	Flags  byte
	Anchor string
}

// RawRef points into cold/raw when retention is on; the zero value means
// no raw copy is stored.
type RawRef struct {
	Seq uint64
	Off uint32
	Len uint32
}

// PageRow is one page across all fifteen columns.
type PageRow struct {
	URLFP    [16]byte
	URL      string
	FetchMS  uint64
	Status   uint16
	Flags    uint32
	SHA256   [32]byte
	Simhash  uint64
	Lang     byte
	CanonFP  [16]byte
	Hdr      []byte
	Title    string
	Text     string
	Outlinks []Outlink
	RawRef   RawRef
	LawVer   uint16
}

const pageNCols = 15

// pageColWant is the compression each column asks for; appendBlock falls
// back to stored whenever zstd does not pay, which is how fixed-width
// columns end up comp 0.
var pageColWant = [pageNCols]byte{
	1:  CompZstd,     // url
	9:  CompZstd,     // hdr
	10: CompZstdDict, // title
	11: CompZstdDict, // text
	12: CompZstdDict, // outlinks
}

// PageSegmentWriter accumulates rows and seals them into one segment.
// BlockTarget overrides BlockRawTarget when nonzero; the cold-block-size
// lab sweeps it, everything else leaves it alone.
type PageSegmentWriter struct {
	Partition   uint16
	Epoch       uint32
	Seq         uint64
	Writer      uint64
	BlockTarget int
	rows        []PageRow
}

func (w *PageSegmentWriter) Add(r PageRow) { w.rows = append(w.rows, r) }

func (w *PageSegmentWriter) NRows() int { return len(w.rows) }

type blockEntry struct {
	offset    uint64
	storedlen uint32
	rawlen    uint32
	firstrow  uint64
	crc       uint32
}

// Seal encodes the segment. Rows must already be in fetch-completion
// order; that is a format property, not something Seal repairs.
func (w *PageSegmentWriter) Seal() ([]byte, error) {
	if len(w.rows) == 0 {
		return nil, errors.New("coldfmt: empty page segment")
	}
	if len(w.rows) > maxSegmentRows {
		return nil, fmt.Errorf("coldfmt: %d rows exceeds segment cap", len(w.rows))
	}
	for i := 1; i < len(w.rows); i++ {
		if w.rows[i].FetchMS < w.rows[i-1].FetchMS {
			return nil, errors.New("coldfmt: rows not in fetch-completion order")
		}
	}

	codec, err := newBlockCodec(w.trainDict())
	if err != nil {
		return nil, err
	}
	defer codec.close()

	target := w.BlockTarget
	if target == 0 {
		target = BlockRawTarget
	}
	out := AppendHeader(make([]byte, 0, 1<<20), FormatPageSeg, w.Writer)
	var index [pageNCols][]blockEntry
	for col := range pageNCols {
		var buf []byte
		firstrow := 0
		blockStart := true
		flush := func(end int) {
			offset := uint64(len(out))
			var storedlen uint32
			out, storedlen = codec.appendBlock(out, buf, pageColWant[col])
			index[col] = append(index[col], blockEntry{
				offset:    offset,
				storedlen: storedlen,
				rawlen:    uint32(len(buf)),
				firstrow:  uint64(firstrow),
				crc:       binary.LittleEndian.Uint32(out[len(out)-4:]),
			})
			buf, firstrow, blockStart = nil, end, true
		}
		for i := range w.rows {
			buf = appendPageCell(buf, col, &w.rows[i], blockStart, i, w.rows)
			blockStart = false
			if len(buf) >= target && i+1 < len(w.rows) {
				flush(i + 1)
			}
		}
		flush(len(w.rows))
	}

	dictOff := uint64(len(out))
	dictBytes := codecDict(codec)
	out = append(out, dictBytes...)

	footerOff := uint64(len(out))
	footer := w.encodeFooter(index[:], dictOff, uint32(len(dictBytes)))
	out = append(out, footer...)
	return AppendTail(out, footerOff, uint32(len(footer))), nil
}

// codecDict returns the dictionary the codec was built with, empty when
// training was skipped.
func codecDict(c *blockCodec) []byte { return c.dict }

// trainDict trains the 64 KiB shared dictionary on the text column. Small
// or degenerate corpora fail training, and that is fine: the segment then
// carries no dictionary and the dict columns fall back to plain zstd.
// The builder also panics outright on inputs it dislikes (seen on real
// Common Crawl text by the zstd-dict lab, slice bounds out of range in
// v1.19.0), so a failed build is caught either way.
func (w *PageSegmentWriter) trainDict() (d []byte) {
	defer func() {
		if recover() != nil {
			d = nil
		}
	}()
	return w.trainDictInner()
}

func (w *PageSegmentWriter) trainDictInner() []byte {
	var samples [][]byte
	for i := range w.rows {
		if t := w.rows[i].Text; t != "" {
			samples = append(samples, []byte(t))
		}
		if len(samples) == 1024 {
			break
		}
	}
	if len(samples) < 8 {
		return nil
	}
	d, err := dict.BuildZstdDict(samples, dict.Options{
		MaxDictSize: DictSize,
		HashBytes:   6,
		ZstdDictID:  0x7A04,
	})
	if err != nil {
		return nil
	}
	return d
}

// appendPageCell appends row i's cell for one column. fetch_ms is
// delta-coded with the chain restarting at every block boundary, so each
// block decodes independently; everything else is stateless per row.
func appendPageCell(dst []byte, col int, r *PageRow, blockStart bool, i int, rows []PageRow) []byte {
	switch col {
	case 0:
		return append(dst, r.URLFP[:]...)
	case 1:
		return AppendString(dst, r.URL)
	case 2:
		if blockStart {
			return AppendUvarint(dst, r.FetchMS)
		}
		return AppendUvarint(dst, r.FetchMS-rows[i-1].FetchMS)
	case 3:
		return binary.LittleEndian.AppendUint16(dst, r.Status)
	case 4:
		return binary.LittleEndian.AppendUint32(dst, r.Flags)
	case 5:
		return append(dst, r.SHA256[:]...)
	case 6:
		return binary.LittleEndian.AppendUint64(dst, r.Simhash)
	case 7:
		return append(dst, r.Lang)
	case 8:
		return append(dst, r.CanonFP[:]...)
	case 9:
		return AppendBytes(dst, r.Hdr)
	case 10:
		return AppendString(dst, r.Title)
	case 11:
		return AppendString(dst, r.Text)
	case 12:
		dst = AppendUvarint(dst, uint64(len(r.Outlinks)))
		for _, l := range r.Outlinks {
			dst = append(dst, l.Dst[:]...)
			dst = append(dst, l.Flags)
			a := l.Anchor
			if len(a) > MaxAnchorLen {
				a = a[:MaxAnchorLen]
			}
			dst = AppendUvarint(dst, uint64(len(a)))
			dst = append(dst, a...)
		}
		return dst
	case 13:
		dst = binary.LittleEndian.AppendUint64(dst, r.RawRef.Seq)
		dst = binary.LittleEndian.AppendUint32(dst, r.RawRef.Off)
		return binary.LittleEndian.AppendUint32(dst, r.RawRef.Len)
	default: // 14
		return binary.LittleEndian.AppendUint16(dst, r.LawVer)
	}
}

func (w *PageSegmentWriter) encodeFooter(index [][]blockEntry, dictOff uint64, dictLen uint32) []byte {
	f := binary.LittleEndian.AppendUint16(nil, w.Partition)
	f = binary.LittleEndian.AppendUint32(f, w.Epoch)
	f = binary.LittleEndian.AppendUint64(f, w.Seq)
	f = binary.LittleEndian.AppendUint64(f, uint64(len(w.rows)))
	f = binary.LittleEndian.AppendUint64(f, w.rows[0].FetchMS)
	f = binary.LittleEndian.AppendUint64(f, w.rows[len(w.rows)-1].FetchMS)
	f = binary.LittleEndian.AppendUint32(f, pageNCols)
	for col, blocks := range index {
		f = binary.LittleEndian.AppendUint16(f, uint16(col))
		f = binary.LittleEndian.AppendUint32(f, uint32(len(blocks)))
		for _, b := range blocks {
			f = binary.LittleEndian.AppendUint64(f, b.offset)
			f = binary.LittleEndian.AppendUint32(f, b.storedlen)
			f = binary.LittleEndian.AppendUint32(f, b.rawlen)
			f = binary.LittleEndian.AppendUint64(f, b.firstrow)
			f = binary.LittleEndian.AppendUint32(f, b.crc)
		}
	}
	f = binary.LittleEndian.AppendUint64(f, dictOff)
	f = binary.LittleEndian.AppendUint32(f, dictLen)
	bloom := bloomBuild(w.rows)
	f = append(f, bloom...)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(bloom)))
	return binary.LittleEndian.AppendUint32(f, CRC(f))
}
