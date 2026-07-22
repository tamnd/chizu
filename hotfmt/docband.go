package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"

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

// DocBandWriter accumulates records in local docid order and seals
// them into the band. Records are buffered raw; compression and
// dictionary training happen once at Seal.
type DocBandWriter struct {
	recs []DocRecord
}

func (w *DocBandWriter) Add(r DocRecord) error {
	if err := r.validate(); err != nil {
		return err
	}
	w.recs = append(w.recs, r)
	return nil
}

// trainDocDict builds the shared dictionary as raw content: records
// stride-sampled across the band, concatenated up to the size cap.
// A raw dictionary is a pure function of the records, which B2
// (bit-identical builds) demands; the structured zstd trainer sorts
// hash buckets out of map iteration order and can never be. Templated
// corpora still share their URL prefixes and boilerplate through the
// raw content. Small bands ship without a dictionary: dict_len 0.
func trainDocDict(recs []DocRecord) []byte {
	if len(recs) < docDictMinSamples {
		return nil
	}
	stride := max(1, len(recs)/docDictSamples)
	var d []byte
	for i := 0; i < len(recs) && len(d) < docDictMaxSize; i += stride {
		d = appendDocRecord(d, &recs[i])
	}
	if len(d) > docDictMaxSize {
		d = d[:docDictMaxSize]
	}
	return d
}

func (w *DocBandWriter) Seal() ([]byte, error) {
	if len(w.recs) == 0 {
		return nil, errors.New("hotfmt: doc band with no records")
	}
	d := trainDocDict(w.recs)
	// Concurrency pinned to 1: band bytes must be a pure function of
	// the records (B2).
	opts := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
	if d != nil {
		opts = append(opts, zstd.WithEncoderDictRaw(formatHot, d))
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = enc.Close() }()

	var out []byte
	var offs []uint64
	var raw []byte
	for start := 0; start < len(w.recs); start += docBlockDocs {
		end := min(start+docBlockDocs, len(w.recs))
		raw = raw[:0]
		for i := start; i < end; i++ {
			raw = appendDocRecord(raw, &w.recs[i])
		}
		offs = append(offs, uint64(len(out)))
		out = enc.EncodeAll(raw, out)
	}
	dictOff := uint64(len(out))
	out = append(out, d...)
	for _, o := range offs {
		if o > maxU48 {
			return nil, errors.New("hotfmt: doc block offset exceeds u48")
		}
		out = appendU48(out, o)
	}
	out = binary.LittleEndian.AppendUint64(out, dictOff)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(d)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(offs)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(w.recs)))
	return out, nil
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
