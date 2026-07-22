package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// FileWriter is the streaming twin of EncodeFile: same canonical byte
// layout, but the big bands arrive as readers and flow straight to the
// destination, so the builder (doc 06) never holds postings, positions,
// or the doc band in memory. Given the same contents the two produce
// identical bytes, which the tests pin.
type FileWriter struct {
	w       io.Writer
	off     uint64
	next    int // index into bandOrder
	entries []bandEntry
	buf     []byte
}

// NewFileWriter validates the header trio the same way EncodeFile does,
// writes the header page, then the meta and fieldstats bands. The
// caller supplies the remaining bands with WriteBand, in canonical
// order, and seals with Finish.
func NewFileWriter(w io.Writer, h *FileHeader, meta *Meta, stats *FieldStats) (*FileWriter, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}
	if meta.DocCount != h.DocCount || meta.TokenizerVer != h.TokenizerVer {
		return nil, errors.New("hotfmt: meta band disagrees with header")
	}
	if f32bits(meta.QuantScale) != f32bits(h.QuantScale) {
		return nil, errors.New("hotfmt: meta quant scale disagrees with header")
	}
	if err := checkFieldsAgree(meta, stats); err != nil {
		return nil, err
	}
	metaBytes, err := EncodeMeta(meta)
	if err != nil {
		return nil, err
	}
	statsBytes, err := EncodeFieldStats(stats)
	if err != nil {
		return nil, err
	}
	fw := &FileWriter{w: w, buf: make([]byte, 32<<10)}
	if err := fw.write(appendFileHeader(nil, h)); err != nil {
		return nil, err
	}
	if err := fw.WriteBandBytes(BandMeta, metaBytes); err != nil {
		return nil, err
	}
	if err := fw.WriteBandBytes(BandFieldstats, statsBytes); err != nil {
		return nil, err
	}
	return fw, nil
}

func (fw *FileWriter) write(b []byte) error {
	n, err := fw.w.Write(b)
	fw.off += uint64(n)
	return err
}

// pad writes the zero bytes up to the next align boundary.
func (fw *FileWriter) pad() error {
	n := int(alignUp(fw.off) - fw.off)
	for n > 0 {
		chunk := min(n, len(fw.buf))
		clear(fw.buf[:chunk])
		if err := fw.write(fw.buf[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// WriteBand streams one band from r. Bands must arrive in canonical
// order (dict, docvalues, tombstones, postings, skips, positions,
// docband after the two NewFileWriter writes); an empty band is an
// empty reader.
func (fw *FileWriter) WriteBand(id byte, r io.Reader) error {
	if fw.next >= numBands || bandOrder[fw.next] != id {
		return fmt.Errorf("hotfmt: band %d out of canonical write order", id)
	}
	if err := fw.pad(); err != nil {
		return err
	}
	off := fw.off
	sum := uint32(0)
	var n uint64
	for {
		k, err := r.Read(fw.buf)
		if k > 0 {
			sum = crc32.Update(sum, castagnoli, fw.buf[:k])
			n += uint64(k)
			if werr := fw.write(fw.buf[:k]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	fw.entries = append(fw.entries, bandEntry{id: id, off: off, len: n, rawlen: n, crc: sum})
	fw.next++
	return nil
}

// WriteBandBytes writes one in-memory band.
func (fw *FileWriter) WriteBandBytes(id byte, payload []byte) error {
	if fw.next >= numBands || bandOrder[fw.next] != id {
		return fmt.Errorf("hotfmt: band %d out of canonical write order", id)
	}
	if err := fw.pad(); err != nil {
		return err
	}
	e := bandEntry{id: id, off: fw.off, len: uint64(len(payload)), rawlen: uint64(len(payload)), crc: crc(payload)}
	if err := fw.write(payload); err != nil {
		return err
	}
	fw.entries = append(fw.entries, e)
	fw.next++
	return nil
}

// Finish writes the footer and tail. Every band must have been written.
func (fw *FileWriter) Finish(prov *Provenance) error {
	if fw.next != numBands {
		return fmt.Errorf("hotfmt: finish after %d of %d bands", fw.next, numBands)
	}
	if len(prov.Watermarks) > 255 {
		return errors.New("hotfmt: provenance watermarks exceed u8 count")
	}
	if len(prov.Stats) > maxFooterStats {
		return errors.New("hotfmt: footer stats block exceeds cap")
	}
	if err := fw.pad(); err != nil {
		return err
	}
	footerOff := fw.off
	out := []byte{numBands}
	for _, e := range fw.entries {
		out = append(out, e.id)
		out = binary.LittleEndian.AppendUint64(out, e.off)
		out = binary.LittleEndian.AppendUint64(out, e.len)
		out = binary.LittleEndian.AppendUint64(out, e.rawlen)
		out = binary.LittleEndian.AppendUint32(out, e.crc)
	}
	out = binary.LittleEndian.AppendUint64(out, prov.Builder)
	out = binary.LittleEndian.AppendUint64(out, prov.BuildMS)
	out = append(out, byte(len(prov.Watermarks)))
	for _, w := range prov.Watermarks {
		out = binary.LittleEndian.AppendUint64(out, w)
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(prov.Stats)))
	out = append(out, prov.Stats...)
	out = binary.LittleEndian.AppendUint32(out, crc(out))

	tail := binary.LittleEndian.AppendUint64(nil, footerOff)
	tail = binary.LittleEndian.AppendUint32(tail, uint32(len(out)))
	tail = binary.LittleEndian.AppendUint32(tail, crc(tail))
	if err := fw.write(out); err != nil {
		return err
	}
	return fw.write(tail)
}
