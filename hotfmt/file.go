package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// File assembly and the open sequence, doc 05 sections 1 and 11. The
// layout is canonical: header page, bands in bandOrder each starting on
// an align boundary with zero padding between, footer, 16-byte tail.
// Given the same contents there is exactly one valid byte stream, which
// is what H-I1's bit-identical reproducibility stands on.

// bandEntry is one footer band index record: band u8, offset u64,
// len u64, rawlen u64, crc u32.
const bandEntrySize = 29

type bandEntry struct {
	id     byte
	off    uint64
	len    uint64
	rawlen uint64
	crc    uint32
}

// maxFooterStats bounds the opaque footer stats block.
const maxFooterStats = 1 << 20

// Provenance is the footer's build record: which cold manifests the
// build consumed, who built it, and when.
type Provenance struct {
	Builder    uint64
	BuildMS    uint64
	Watermarks []uint64
	Stats      []byte // opaque stats block; later slices define it
}

// EncodeFile assembles a complete .hot file. Meta and fieldstats are
// encoded from their structs; the other seven bands arrive as opaque
// payloads (their codecs are the next slice), any of them absent for
// an empty band. The builder proper (doc 06) will stream instead of
// holding bands in memory; the byte layout it must produce is this one.
func EncodeFile(h *FileHeader, meta *Meta, stats *FieldStats, bands map[byte][]byte, prov *Provenance) ([]byte, error) {
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
	if len(prov.Watermarks) > 255 {
		return nil, errors.New("hotfmt: provenance watermarks exceed u8 count")
	}
	if len(prov.Stats) > maxFooterStats {
		return nil, errors.New("hotfmt: footer stats block exceeds cap")
	}
	for id := range bands {
		if id == BandMeta || id == BandFieldstats {
			return nil, errors.New("hotfmt: meta and fieldstats bands come from their structs")
		}
		if id >= numBands {
			return nil, fmt.Errorf("hotfmt: unknown band %d", id)
		}
	}
	metaBytes, err := EncodeMeta(meta)
	if err != nil {
		return nil, err
	}
	statsBytes, err := EncodeFieldStats(stats)
	if err != nil {
		return nil, err
	}

	out := appendFileHeader(nil, h)
	entries := make([]bandEntry, 0, numBands)
	for _, id := range bandOrder {
		var payload []byte
		switch id {
		case BandMeta:
			payload = metaBytes
		case BandFieldstats:
			payload = statsBytes
		default:
			payload = bands[id]
		}
		out = append(out, make([]byte, alignUp(uint64(len(out)))-uint64(len(out)))...)
		entries = append(entries, bandEntry{
			id:     id,
			off:    uint64(len(out)),
			len:    uint64(len(payload)),
			rawlen: uint64(len(payload)),
			crc:    crc(payload),
		})
		out = append(out, payload...)
	}
	out = append(out, make([]byte, alignUp(uint64(len(out)))-uint64(len(out)))...)

	footerOff := uint64(len(out))
	out = append(out, numBands)
	for _, e := range entries {
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
	out = binary.LittleEndian.AppendUint32(out, crc(out[footerOff:]))

	out = binary.LittleEndian.AppendUint64(out, footerOff)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(out)-8-int(footerOff)))
	return binary.LittleEndian.AppendUint32(out, crc(out[len(out)-12:])), nil
}

func checkFieldsAgree(meta *Meta, stats *FieldStats) error {
	if len(meta.Fields) != len(stats.Fields) {
		return errors.New("hotfmt: meta and fieldstats field counts differ")
	}
	for i := range meta.Fields {
		if meta.Fields[i].ID != stats.Fields[i].ID {
			return errors.New("hotfmt: meta and fieldstats field ids differ")
		}
	}
	return nil
}

// Shard is an opened .hot file: header, resident band structs, band
// index, provenance. The big bands stay on the ReaderAt and are read
// through ReadBand; the serving pread planner (doc 07) will read them
// in aligned block ranges instead of whole.
type Shard struct {
	Header     FileHeader
	Meta       *Meta
	Stats      *FieldStats
	Provenance Provenance

	r     io.ReaderAt
	bands [numBands]bandEntry
}

// Open runs the doc 05 section 11 sequence: pread the tail, pread the
// footer, verify the band index invariants, pread the header page,
// decode the two RAM bands and cross-check them against the header.
// Band payload crcs are verified lazily, on ReadBand, per the spec;
// download-time full verification is VerifyBands.
func Open(r io.ReaderAt, size int64) (*Shard, error) {
	if size < hotHeaderSize+align+tailSize {
		return nil, errors.New("hotfmt: file too small")
	}
	tail := make([]byte, tailSize)
	if _, err := r.ReadAt(tail, size-tailSize); err != nil {
		return nil, fmt.Errorf("hotfmt: read tail: %w", err)
	}
	if binary.LittleEndian.Uint32(tail[12:]) != crc(tail[:12]) {
		return nil, errors.New("hotfmt: tail crc mismatch")
	}
	footerOff := binary.LittleEndian.Uint64(tail)
	footerLen := binary.LittleEndian.Uint32(tail[8:])
	if footerOff%align != 0 || footerOff+uint64(footerLen) != uint64(size)-tailSize {
		return nil, errors.New("hotfmt: footer extent does not fit the file")
	}
	footer := make([]byte, footerLen)
	if _, err := r.ReadAt(footer, int64(footerOff)); err != nil {
		return nil, fmt.Errorf("hotfmt: read footer: %w", err)
	}
	s := &Shard{r: r}
	if err := s.parseFooter(footer, footerOff); err != nil {
		return nil, err
	}

	head := make([]byte, hotHeaderSize)
	if _, err := r.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("hotfmt: read header: %w", err)
	}
	h, err := parseFileHeader(head)
	if err != nil {
		return nil, err
	}
	s.Header = h

	metaBytes, err := s.ReadBand(BandMeta)
	if err != nil {
		return nil, err
	}
	if s.Meta, err = ParseMeta(metaBytes); err != nil {
		return nil, err
	}
	statsBytes, err := s.ReadBand(BandFieldstats)
	if err != nil {
		return nil, err
	}
	if s.Stats, err = ParseFieldStats(statsBytes); err != nil {
		return nil, err
	}
	if s.Meta.DocCount != h.DocCount || s.Meta.TokenizerVer != h.TokenizerVer {
		return nil, errors.New("hotfmt: meta band disagrees with header")
	}
	if f32bits(s.Meta.QuantScale) != f32bits(h.QuantScale) {
		return nil, errors.New("hotfmt: meta quant scale disagrees with header")
	}
	if err := checkFieldsAgree(s.Meta, s.Stats); err != nil {
		return nil, err
	}
	return s, nil
}

// parseFooter decodes the band index and provenance, holding the layout
// to the canonical shape: all nine bands present in bandOrder, offsets
// dense from the first align boundary with only alignment padding
// between, and the footer starting where the last padded band ends.
func (s *Shard) parseFooter(footer []byte, footerOff uint64) error {
	if len(footer) < 1+numBands*bandEntrySize+8+8+1+4+4 {
		return errors.New("hotfmt: footer too short")
	}
	body := footer[:len(footer)-4]
	if binary.LittleEndian.Uint32(footer[len(footer)-4:]) != crc(body) {
		return errors.New("hotfmt: footer crc mismatch")
	}
	if body[0] != numBands {
		return fmt.Errorf("hotfmt: %d bands, want %d", body[0], numBands)
	}
	next := uint64(align)
	pos := 1
	for i, id := range bandOrder {
		e := bandEntry{
			id:     body[pos],
			off:    binary.LittleEndian.Uint64(body[pos+1:]),
			len:    binary.LittleEndian.Uint64(body[pos+9:]),
			rawlen: binary.LittleEndian.Uint64(body[pos+17:]),
			crc:    binary.LittleEndian.Uint32(body[pos+25:]),
		}
		pos += bandEntrySize
		if e.id != id {
			return fmt.Errorf("hotfmt: band %d out of canonical order", e.id)
		}
		if e.off != next {
			return fmt.Errorf("hotfmt: band %d at offset %d, want %d", e.id, e.off, next)
		}
		if e.len > uint64(footerOff)-e.off {
			return fmt.Errorf("hotfmt: band %d overruns the footer", e.id)
		}
		// No band compresses at the file level yet; rawlen == len until a
		// band codec defines otherwise.
		if e.rawlen != e.len {
			return fmt.Errorf("hotfmt: band %d rawlen %d != len %d", e.id, e.rawlen, e.len)
		}
		s.bands[i] = e
		next = alignUp(e.off + e.len)
	}
	if next != footerOff {
		return errors.New("hotfmt: gap between last band and footer")
	}
	rest := body[pos:]
	if len(rest) < 8+8+1 {
		return errors.New("hotfmt: footer provenance truncated")
	}
	s.Provenance.Builder = binary.LittleEndian.Uint64(rest)
	s.Provenance.BuildMS = binary.LittleEndian.Uint64(rest[8:])
	nwm := int(rest[16])
	rest = rest[17:]
	if len(rest) < nwm*8+4 {
		return errors.New("hotfmt: footer watermarks truncated")
	}
	s.Provenance.Watermarks = make([]uint64, 0, nwm)
	for i := range nwm {
		s.Provenance.Watermarks = append(s.Provenance.Watermarks, binary.LittleEndian.Uint64(rest[i*8:]))
	}
	rest = rest[nwm*8:]
	statsLen := binary.LittleEndian.Uint32(rest)
	if statsLen > maxFooterStats {
		return errors.New("hotfmt: footer stats block exceeds cap")
	}
	if uint64(len(rest)-4) != uint64(statsLen) {
		return errors.New("hotfmt: footer stats length mismatch")
	}
	s.Provenance.Stats = rest[4:]
	return nil
}

// Band reports one band's file extent for a caller planning its own
// ranged reads.
func (s *Shard) Band(id byte) (off, length uint64, err error) {
	e, err := s.entry(id)
	if err != nil {
		return 0, 0, err
	}
	return e.off, e.len, nil
}

// ReadBand preads one whole band and verifies its crc: the lazy half of
// the open sequence, and the right call for the small resident bands.
func (s *Shard) ReadBand(id byte) ([]byte, error) {
	e, err := s.entry(id)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, e.len)
	if _, err := s.r.ReadAt(payload, int64(e.off)); err != nil {
		return nil, fmt.Errorf("hotfmt: read band %d: %w", id, err)
	}
	if crc(payload) != e.crc {
		return nil, fmt.Errorf("hotfmt: band %d crc mismatch", id)
	}
	return payload, nil
}

// VerifyBands reads and crc-checks every band: the download-time full
// verification of doc 02, off the open path.
func (s *Shard) VerifyBands() error {
	for _, id := range bandOrder {
		if _, err := s.ReadBand(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Shard) entry(id byte) (bandEntry, error) {
	for _, e := range s.bands {
		if e.id == id {
			return e, nil
		}
	}
	return bandEntry{}, fmt.Errorf("hotfmt: unknown band %d", id)
}
