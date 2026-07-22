// Package build is the index-build plane of doc 06: shard pass (tokenize
// in docid order into sorted postings runs, docvalues and doc band in
// stream) and emit pass (k-way merge into a sealed .hot). Only cmd/chizu
// imports it.
package build

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Run file: the shard pass's spill unit (doc 06 section 7), a sorted
// stream of postings records behind one zstd frame. Runs are build-node
// scratch, never uploaded, but the emit pass parses them back, so the
// codec carries the same defensive limits as the at-rest formats.

// runMagic starts every run file; the byte after the magic is the run
// format version.
var runMagic = []byte{'C', 'Z', 'R', 'N', 1}

// NumFields is the field count this generation: 0 body, 1 title.
const NumFields = 2

const (
	// maxRecTerm caps a decoded term length; the tokenizer admits at
	// most 64 bytes, the codec allows slack for future versions.
	maxRecTerm = 1024
	// maxRecPositions caps a decoded position list; positions are
	// uint16 so a field holds at most 65536 of them.
	maxRecPositions = 1 << 16
)

// Rec is one postings record: one term in one doc, tf and positions
// split per field.
type Rec struct {
	Term  []byte
	Docid uint32
	TF    uint32
	Mask  uint8
	Pos   [NumFields][]uint16
}

// appendRec encodes r: term length and bytes, docid, tf, field mask,
// then per set field a count and delta-coded positions.
func appendRec(dst []byte, r *Rec) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(r.Term)))
	dst = append(dst, r.Term...)
	dst = binary.AppendUvarint(dst, uint64(r.Docid))
	dst = binary.AppendUvarint(dst, uint64(r.TF))
	dst = append(dst, r.Mask)
	for f := range NumFields {
		if r.Mask&(1<<f) == 0 {
			continue
		}
		dst = binary.AppendUvarint(dst, uint64(len(r.Pos[f])))
		prev := uint16(0)
		for _, p := range r.Pos[f] {
			dst = binary.AppendUvarint(dst, uint64(p-prev))
			prev = p
		}
	}
	return dst
}

// readRec decodes the next record into r, reusing its slices. The
// caller owns r's memory between calls; io.EOF means a clean end
// between records.
func readRec(br *bufio.Reader, r *Rec) error {
	tl, err := binary.ReadUvarint(br)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		return fmt.Errorf("build: run term length: %w", err)
	}
	if tl == 0 || tl > maxRecTerm {
		return fmt.Errorf("build: run term length %d out of range", tl)
	}
	r.Term = resize(r.Term, int(tl))
	if _, err := io.ReadFull(br, r.Term); err != nil {
		return fmt.Errorf("build: run term bytes: %w", err)
	}
	docid, err := binary.ReadUvarint(br)
	if err != nil {
		return fmt.Errorf("build: run docid: %w", err)
	}
	if docid > 0xFFFFFFFF {
		return errors.New("build: run docid overflows uint32")
	}
	r.Docid = uint32(docid)
	tf, err := binary.ReadUvarint(br)
	if err != nil {
		return fmt.Errorf("build: run tf: %w", err)
	}
	if tf == 0 || tf > 0xFFFFFFFF {
		return fmt.Errorf("build: run tf %d out of range", tf)
	}
	r.TF = uint32(tf)
	mask, err := br.ReadByte()
	if err != nil {
		return fmt.Errorf("build: run mask: %w", err)
	}
	if mask == 0 || mask >= 1<<NumFields {
		return fmt.Errorf("build: run field mask %#x out of range", mask)
	}
	r.Mask = mask
	for f := range NumFields {
		r.Pos[f] = r.Pos[f][:0]
		if mask&(1<<f) == 0 {
			continue
		}
		n, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("build: run position count: %w", err)
		}
		if n == 0 || n > maxRecPositions {
			return fmt.Errorf("build: run position count %d out of range", n)
		}
		pos := uint64(0)
		for range n {
			d, err := binary.ReadUvarint(br)
			if err != nil {
				return fmt.Errorf("build: run position delta: %w", err)
			}
			pos += d
			if pos > 0xFFFF {
				return errors.New("build: run position overflows uint16")
			}
			r.Pos[f] = append(r.Pos[f], uint16(pos))
		}
	}
	return nil
}

func resize(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

// RunWriter streams sorted records into one zstd-framed run file.
type RunWriter struct {
	f   *os.File
	zw  *zstd.Encoder
	buf []byte
}

// NewRunWriter creates path and writes the header.
func NewRunWriter(path string) (*RunWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(runMagic); err != nil {
		f.Close()
		return nil, err
	}
	// One concurrent frame: run bytes must be a pure function of the
	// records for the determinism harness, and encoder concurrency
	// could split frames by timing.
	zw, err := zstd.NewWriter(f, zstd.WithEncoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RunWriter{f: f, zw: zw}, nil
}

// Add appends one record.
func (w *RunWriter) Add(r *Rec) error {
	w.buf = appendRec(w.buf[:0], r)
	_, err := w.zw.Write(w.buf)
	return err
}

// Close seals the zstd frame and the file.
func (w *RunWriter) Close() error {
	zerr := w.zw.Close()
	ferr := w.f.Close()
	if zerr != nil {
		return zerr
	}
	return ferr
}

// RunReader streams records back out of a run file.
type RunReader struct {
	f  *os.File
	zr *zstd.Decoder
	br *bufio.Reader
}

// OpenRun opens path and checks the header.
func OpenRun(path string) (*RunReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var magic [5]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		f.Close()
		return nil, fmt.Errorf("build: run header: %w", err)
	}
	if string(magic[:]) != string(runMagic) {
		f.Close()
		return nil, errors.New("build: not a run file")
	}
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RunReader{f: f, zr: zr, br: bufio.NewReader(zr)}, nil
}

// Next decodes the next record into r, reusing r's slices; io.EOF ends
// the run.
func (r *RunReader) Next(rec *Rec) error {
	return readRec(r.br, rec)
}

// Close releases the decoder and the file.
func (r *RunReader) Close() error {
	r.zr.Close()
	return r.f.Close()
}
