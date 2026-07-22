package hotfmt

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Doc band, doc 05 section 9: per-doc display records in 64-doc zstd
// blocks behind a resident offset array, so serving a result page is
// at most ten one-block preads. Layout: blocks, then the trained
// dictionary, then the u48 offset array, then a 20-byte trailer
// (dict_off u64, dict_len u32, nblocks u32, ndocs u32).

const (
	docBlockDocs       = 64
	docBandTrailerSize = 20
	docFPSize          = 16
	maxDocField        = 1<<16 - 1

	docDictMaxSize    = 64 << 10
	docDictMinSamples = 8
	docDictSamples    = 256
)

// DocRecord is one doc's display record: what serving needs to render
// a result without touching a page body.
type DocRecord struct {
	URL     []byte
	Title   []byte
	Snippet []byte
	URLFP   [16]byte
}

func (r *DocRecord) validate() error {
	if len(r.URL) == 0 {
		return errors.New("hotfmt: doc record with empty url")
	}
	if len(r.URL) > maxDocField || len(r.Title) > maxDocField || len(r.Snippet) > maxDocField {
		return errors.New("hotfmt: doc record field exceeds u16 length")
	}
	return nil
}

func appendDocRecord(dst []byte, r *DocRecord) []byte {
	dst = appendUvarint(dst, uint64(len(r.URL)))
	dst = append(dst, r.URL...)
	dst = appendUvarint(dst, uint64(len(r.Title)))
	dst = append(dst, r.Title...)
	dst = appendUvarint(dst, uint64(len(r.Snippet)))
	dst = append(dst, r.Snippet...)
	return append(dst, r.URLFP[:]...)
}

func parseDocRecord(data []byte) (DocRecord, int, error) {
	var r DocRecord
	pos := 0
	for _, f := range []*[]byte{&r.URL, &r.Title, &r.Snippet} {
		l, n, err := uvarint(data[pos:])
		if err != nil {
			return r, 0, err
		}
		if l > maxDocField {
			return r, 0, errors.New("hotfmt: doc record field exceeds u16 length")
		}
		pos += n
		if uint64(len(data)-pos) < l {
			return r, 0, errors.New("hotfmt: doc record truncated")
		}
		*f = data[pos : pos+int(l) : pos+int(l)]
		pos += int(l)
	}
	if len(data)-pos < docFPSize {
		return r, 0, errors.New("hotfmt: doc record truncated")
	}
	r.URLFP = [16]byte(data[pos : pos+docFPSize])
	pos += docFPSize
	if len(r.URL) == 0 {
		return r, 0, errors.New("hotfmt: doc record with empty url")
	}
	return r, pos, nil
}

// DocBandWriter spools encoded records to disk in local docid order
// and seals them into the band with two sequential passes over the
// spool, so the band build holds one block of records in memory no
// matter the doc count. The resident cost at Seal is the block offset
// array: 8 bytes per 64 docs, about 12 MiB at 100M.
type DocBandWriter struct {
	dir     string
	f       *os.File
	bw      *bufio.Writer
	scratch []byte
	n       uint32
	err     error
}

// NewDocBandWriter spools into dir; the spool file appears on the
// first Add and is removed by Seal.
func NewDocBandWriter(dir string) *DocBandWriter {
	return &DocBandWriter{dir: dir}
}

func (w *DocBandWriter) Add(r DocRecord) error {
	if w.err != nil {
		return w.err
	}
	if err := r.validate(); err != nil {
		return err
	}
	if w.f == nil {
		f, err := os.CreateTemp(w.dir, "docband-*.spool")
		if err != nil {
			w.err = err
			return err
		}
		w.f = f
		w.bw = bufio.NewWriterSize(f, 1<<20)
	}
	// Each spooled record carries a u32 length prefix so the seal
	// passes replay the exact appendDocRecord bytes without parsing.
	w.scratch = appendDocRecord(w.scratch[:0], &r)
	var pre [4]byte
	binary.LittleEndian.PutUint32(pre[:], uint32(len(w.scratch)))
	if _, err := w.bw.Write(pre[:]); err != nil {
		w.err = err
		return err
	}
	if _, err := w.bw.Write(w.scratch); err != nil {
		w.err = err
		return err
	}
	w.n++
	return nil
}

// records replays the spool in order; fn returning false stops early.
func (w *DocBandWriter) records(fn func(i uint32, rec []byte) (bool, error)) error {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(w.f, 1<<20)
	var pre [4]byte
	var buf []byte
	for i := uint32(0); i < w.n; i++ {
		if _, err := io.ReadFull(br, pre[:]); err != nil {
			return err
		}
		l := binary.LittleEndian.Uint32(pre[:])
		if int(l) > cap(buf) {
			buf = make([]byte, l)
		}
		rec := buf[:l]
		if _, err := io.ReadFull(br, rec); err != nil {
			return err
		}
		cont, err := fn(i, rec)
		if err != nil || !cont {
			return err
		}
	}
	return nil
}

// trainDict builds the shared dictionary as raw content: records
// stride-sampled across the band, concatenated up to the size cap.
// A raw dictionary is a pure function of the records, which B2
// (bit-identical builds) demands; the structured zstd trainer sorts
// hash buckets out of map iteration order and can never be. Templated
// corpora still share their URL prefixes and boilerplate through the
// raw content. Small bands ship without a dictionary: dict_len 0.
func (w *DocBandWriter) trainDict() ([]byte, error) {
	if w.n < docDictMinSamples {
		return nil, nil
	}
	stride := max(1, int(w.n)/docDictSamples)
	var d []byte
	err := w.records(func(i uint32, rec []byte) (bool, error) {
		if int(i)%stride != 0 {
			return true, nil
		}
		if len(d) >= docDictMaxSize {
			return false, nil
		}
		d = append(d, rec...)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if len(d) > docDictMaxSize {
		d = d[:docDictMaxSize]
	}
	return d, nil
}

// Seal streams the band into out: dictionary pass, then block pass,
// then the offset array and trailer. The spool is gone afterwards and
// the writer is spent.
func (w *DocBandWriter) Seal(out io.Writer) error {
	if w.err != nil {
		return w.err
	}
	if w.n == 0 {
		return errors.New("hotfmt: doc band with no records")
	}
	defer func() {
		_ = w.f.Close()
		_ = os.Remove(w.f.Name())
	}()
	if err := w.bw.Flush(); err != nil {
		return err
	}

	d, err := w.trainDict()
	if err != nil {
		return err
	}
	// Concurrency pinned to 1: band bytes must be a pure function of
	// the records (B2).
	opts := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
	if d != nil {
		opts = append(opts, zstd.WithEncoderDictRaw(formatHot, d))
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return err
	}
	defer func() { _ = enc.Close() }()

	ow := bufio.NewWriterSize(out, 1<<20)
	var off uint64
	offs := make([]uint64, 0, (w.n+docBlockDocs-1)/docBlockDocs)
	var raw, blk []byte
	flushBlock := func() error {
		if len(raw) == 0 {
			return nil
		}
		offs = append(offs, off)
		blk = enc.EncodeAll(raw, blk[:0])
		if _, err := ow.Write(blk); err != nil {
			return err
		}
		off += uint64(len(blk))
		raw = raw[:0]
		return nil
	}
	err = w.records(func(i uint32, rec []byte) (bool, error) {
		raw = append(raw, rec...)
		if (i+1)%docBlockDocs == 0 {
			return true, flushBlock()
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if err := flushBlock(); err != nil {
		return err
	}

	dictOff := off
	if _, err := ow.Write(d); err != nil {
		return err
	}
	tail := make([]byte, 0, len(offs)*6+docBandTrailerSize)
	for _, o := range offs {
		if o > maxU48 {
			return errors.New("hotfmt: doc block offset exceeds u48")
		}
		tail = appendU48(tail, o)
	}
	tail = binary.LittleEndian.AppendUint64(tail, dictOff)
	tail = binary.LittleEndian.AppendUint32(tail, uint32(len(d)))
	tail = binary.LittleEndian.AppendUint32(tail, uint32(len(offs)))
	tail = binary.LittleEndian.AppendUint32(tail, w.n)
	if _, err := ow.Write(tail); err != nil {
		return err
	}
	return ow.Flush()
}

// DocBand is the read view: resident offset array, lazy per-block
// decompression on Doc.
type DocBand struct {
	data    []byte
	offs    []uint64
	dictOff uint64
	ndocs   uint32
	dec     *zstd.Decoder
}

// OpenDocBand validates the trailer and offset array and builds the
// decoder. Block payloads decode lazily on Doc.
func OpenDocBand(data []byte) (*DocBand, error) {
	if len(data) < docBandTrailerSize {
		return nil, errors.New("hotfmt: doc band truncated")
	}
	tr := data[len(data)-docBandTrailerSize:]
	dictOff := binary.LittleEndian.Uint64(tr)
	dictLen := binary.LittleEndian.Uint32(tr[8:])
	nblocks := binary.LittleEndian.Uint32(tr[12:])
	ndocs := binary.LittleEndian.Uint32(tr[16:])
	if ndocs == 0 || nblocks == 0 {
		return nil, errors.New("hotfmt: empty doc band")
	}
	if uint64(nblocks) != (uint64(ndocs)+docBlockDocs-1)/docBlockDocs {
		return nil, errors.New("hotfmt: doc band block count disagrees with doc count")
	}
	arrOff := uint64(len(data)) - docBandTrailerSize - uint64(nblocks)*6
	if arrOff > uint64(len(data)) || dictOff+uint64(dictLen) != arrOff {
		return nil, errors.New("hotfmt: doc band trailer geometry broken")
	}
	b := &DocBand{data: data, dictOff: dictOff, ndocs: ndocs}
	b.offs = make([]uint64, nblocks)
	for i := range b.offs {
		b.offs[i] = u48(data[arrOff+uint64(i)*6:])
		if i == 0 && b.offs[0] != 0 {
			return nil, errors.New("hotfmt: first doc block not at offset 0")
		}
		if i > 0 && b.offs[i] <= b.offs[i-1] {
			return nil, errors.New("hotfmt: doc block offsets not strictly increasing")
		}
		if b.offs[i] >= dictOff {
			return nil, errors.New("hotfmt: doc block offset past dictionary")
		}
	}
	// A block is at most 64 records of three u16-length fields plus the
	// fingerprint, so 64 MiB bounds any honest block with two orders of
	// magnitude to spare and turns a decompression bomb into an error.
	opts := []zstd.DOption{zstd.WithDecoderMaxMemory(64 << 20)}
	if dictLen > 0 {
		opts = append(opts, zstd.WithDecoderDictRaw(formatHot, data[dictOff:dictOff+uint64(dictLen)]))
	}
	var err error
	b.dec, err = zstd.NewReader(nil, opts...)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (b *DocBand) NDocs() uint32 { return b.ndocs }

func (b *DocBand) Close() { b.dec.Close() }

// Doc decompresses the containing block and walks to the record. The
// block must hold exactly its share of records with no slack.
func (b *DocBand) Doc(docid uint32) (DocRecord, error) {
	if docid >= b.ndocs {
		return DocRecord{}, fmt.Errorf("hotfmt: docid %d past doc band", docid)
	}
	blk := int(docid / docBlockDocs)
	end := b.dictOff
	if blk+1 < len(b.offs) {
		end = b.offs[blk+1]
	}
	raw, err := b.dec.DecodeAll(b.data[b.offs[blk]:end], nil)
	if err != nil {
		return DocRecord{}, err
	}
	nrecs := min(int(b.ndocs)-blk*docBlockDocs, docBlockDocs)
	var rec DocRecord
	pos := 0
	for i := range nrecs {
		r, n, err := parseDocRecord(raw[pos:])
		if err != nil {
			return DocRecord{}, err
		}
		pos += n
		if i == int(docid%docBlockDocs) {
			rec = r
		}
	}
	if pos != len(raw) {
		return DocRecord{}, errors.New("hotfmt: trailing bytes in doc block")
	}
	return rec, nil
}
