package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frontier checkpoints, doc 04 section 5: a container of sections, each
// independently compressed and crc'd, offsets in the footer. Only the crawl
// plane reads these, and the section codecs live beside their in-RAM
// structures in the frontier package so they evolve together under one
// fversion; to coldfmt every section is opaque bytes. The one exception is
// the fetch ledger, whose row layout is table-frozen below because the
// politeness audit and the corpus-stats suite read it from outside.

// Section ids. The container carries any id; these are the ones doc 04
// names. Full URL table appears on snapshot cadence only.
const (
	SectionURLTableDelta uint16 = 1
	SectionHostQueues    uint16 = 2
	SectionPending       uint16 = 3
	SectionFetchLedger   uint16 = 4
	SectionURLTableFull  uint16 = 5
)

// maxSections caps a declared section count; an honest checkpoint holds a
// handful.
const maxSections = 64

// frontierSectionSize is one footer entry: id u16, offset u64, storedlen
// u32, rawlen u32, crc u32.
const frontierSectionSize = 22

type frontierSection struct {
	id    uint16
	entry blockEntry
}

// FrontierWriter accumulates sections and seals them into one checkpoint.
type FrontierWriter struct {
	Partition uint16
	Epoch     uint32
	Seq       uint64
	Writer    uint64
	ids       []uint16
	raws      [][]byte
}

// AddSection appends one section; ids must be unique within a checkpoint.
func (w *FrontierWriter) AddSection(id uint16, raw []byte) {
	w.ids = append(w.ids, id)
	w.raws = append(w.raws, raw)
}

func (w *FrontierWriter) Seal() ([]byte, error) {
	if len(w.ids) == 0 {
		return nil, errors.New("coldfmt: empty frontier checkpoint")
	}
	if len(w.ids) > maxSections {
		return nil, fmt.Errorf("coldfmt: %d sections exceeds cap", len(w.ids))
	}
	seen := make(map[uint16]bool, len(w.ids))
	for _, id := range w.ids {
		if seen[id] {
			return nil, fmt.Errorf("coldfmt: duplicate section %d", id)
		}
		seen[id] = true
	}

	// No shared dictionary: sections are few, large, and self-contained.
	codec, err := newBlockCodec(nil)
	if err != nil {
		return nil, err
	}
	defer codec.close()

	out := AppendHeader(make([]byte, 0, 1<<16), FormatFrontier, w.Writer)
	sections := make([]frontierSection, len(w.ids))
	for i, raw := range w.raws {
		offset := uint64(len(out))
		var storedlen uint32
		out, storedlen = codec.appendBlock(out, raw, CompZstd)
		sections[i] = frontierSection{id: w.ids[i], entry: blockEntry{
			offset:    offset,
			storedlen: storedlen,
			rawlen:    uint32(len(raw)),
			crc:       binary.LittleEndian.Uint32(out[len(out)-4:]),
		}}
	}

	footerOff := uint64(len(out))
	f := binary.LittleEndian.AppendUint16(nil, w.Partition)
	f = binary.LittleEndian.AppendUint32(f, w.Epoch)
	f = binary.LittleEndian.AppendUint64(f, w.Seq)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(sections)))
	for _, s := range sections {
		f = binary.LittleEndian.AppendUint16(f, s.id)
		f = binary.LittleEndian.AppendUint64(f, s.entry.offset)
		f = binary.LittleEndian.AppendUint32(f, s.entry.storedlen)
		f = binary.LittleEndian.AppendUint32(f, s.entry.rawlen)
		f = binary.LittleEndian.AppendUint32(f, s.entry.crc)
	}
	f = binary.LittleEndian.AppendUint32(f, CRC(f))
	out = append(out, f...)
	return AppendTail(out, footerOff, uint32(len(f))), nil
}

// FrontierCheckpoint is an opened checkpoint.
type FrontierCheckpoint struct {
	Header    Header
	Partition uint16
	Epoch     uint32
	Seq       uint64

	sections []frontierSection
	data     []byte
	codec    *blockCodec
}

func OpenFrontierCheckpoint(data []byte) (*FrontierCheckpoint, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatFrontier {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a frontier checkpoint", h.Format)
	}
	footerOff, footerLen, err := ParseTail(data)
	if err != nil {
		return nil, err
	}
	end := uint64(len(data) - TailSize)
	if footerOff > end || footerOff+uint64(footerLen) != end {
		return nil, errors.New("coldfmt: footer extent out of bounds")
	}
	f := data[footerOff:end]
	if len(f) < 18+frontierSectionSize+4 {
		return nil, errors.New("coldfmt: footer too short")
	}
	if binary.LittleEndian.Uint32(f[len(f)-4:]) != CRC(f[:len(f)-4]) {
		return nil, errors.New("coldfmt: footer crc mismatch")
	}
	c := &FrontierCheckpoint{Header: h, data: data}
	c.Partition = binary.LittleEndian.Uint16(f)
	c.Epoch = binary.LittleEndian.Uint32(f[2:])
	c.Seq = binary.LittleEndian.Uint64(f[6:])
	n := binary.LittleEndian.Uint32(f[14:])
	if n == 0 || n > maxSections {
		return nil, fmt.Errorf("coldfmt: section count %d out of range", n)
	}
	if uint64(len(f)-18-4) != uint64(n)*frontierSectionSize {
		return nil, errors.New("coldfmt: section index length mismatch")
	}
	pos := 18
	c.sections = make([]frontierSection, n)
	seen := make(map[uint16]bool, n)
	for i := range c.sections {
		c.sections[i] = frontierSection{
			id: binary.LittleEndian.Uint16(f[pos:]),
			entry: blockEntry{
				offset:    binary.LittleEndian.Uint64(f[pos+2:]),
				storedlen: binary.LittleEndian.Uint32(f[pos+10:]),
				rawlen:    binary.LittleEndian.Uint32(f[pos+14:]),
				crc:       binary.LittleEndian.Uint32(f[pos+18:]),
			},
		}
		pos += frontierSectionSize
		if seen[c.sections[i].id] {
			return nil, fmt.Errorf("coldfmt: duplicate section %d", c.sections[i].id)
		}
		seen[c.sections[i].id] = true
	}
	c.codec, err = newBlockCodec(nil)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// SectionIDs lists the sections present, in stored order.
func (c *FrontierCheckpoint) SectionIDs() []uint16 {
	ids := make([]uint16, len(c.sections))
	for i, s := range c.sections {
		ids[i] = s.id
	}
	return ids
}

// Section decodes one section; ok is false when the id is absent.
func (c *FrontierCheckpoint) Section(id uint16) (raw []byte, ok bool, err error) {
	for _, s := range c.sections {
		if s.id != id {
			continue
		}
		b := s.entry
		if b.offset > uint64(len(c.data)) ||
			uint64(b.storedlen) > uint64(len(c.data))-b.offset-blockHeaderSize-4 {
			return nil, true, errors.New("coldfmt: section extent out of bounds")
		}
		raw, err = c.codec.decodeBlock(c.data[b.offset : b.offset+blockHeaderSize+uint64(b.storedlen)+4])
		if err != nil {
			return nil, true, err
		}
		if uint32(len(raw)) != b.rawlen {
			return nil, true, errors.New("coldfmt: section rawlen disagrees with index")
		}
		return raw, true, nil
	}
	return nil, false, nil
}

func (c *FrontierCheckpoint) Close() { c.codec.close() }

// The fetch ledger row, table-frozen here (doc 04 section 5): urlfp 16,
// ts u64 ms, status u16, bytes u32, elapsed u32 ms, outcome class u8.
// The outcome class table lives with the crawl plane (doc 03); this layer
// only carries the byte.
type FetchLedgerRow struct {
	URLFP     [16]byte
	TS        uint64
	Status    uint16
	Bytes     uint32
	ElapsedMS uint32
	Outcome   byte
}

const fetchLedgerRowSize = 35

// AppendFetchLedger encodes rows for a SectionFetchLedger payload.
func AppendFetchLedger(dst []byte, rows []FetchLedgerRow) []byte {
	for i := range rows {
		r := &rows[i]
		dst = append(dst, r.URLFP[:]...)
		dst = binary.LittleEndian.AppendUint64(dst, r.TS)
		dst = binary.LittleEndian.AppendUint16(dst, r.Status)
		dst = binary.LittleEndian.AppendUint32(dst, r.Bytes)
		dst = binary.LittleEndian.AppendUint32(dst, r.ElapsedMS)
		dst = append(dst, r.Outcome)
	}
	return dst
}

// DecodeFetchLedger decodes a whole SectionFetchLedger payload.
func DecodeFetchLedger(data []byte) ([]FetchLedgerRow, error) {
	if len(data)%fetchLedgerRowSize != 0 {
		return nil, errors.New("coldfmt: fetch ledger length not a row multiple")
	}
	rows := make([]FetchLedgerRow, len(data)/fetchLedgerRowSize)
	for i := range rows {
		r := &rows[i]
		p := data[i*fetchLedgerRowSize:]
		copy(r.URLFP[:], p)
		r.TS = binary.LittleEndian.Uint64(p[16:])
		r.Status = binary.LittleEndian.Uint16(p[24:])
		r.Bytes = binary.LittleEndian.Uint32(p[26:])
		r.ElapsedMS = binary.LittleEndian.Uint32(p[30:])
		r.Outcome = p[34]
	}
	return rows, nil
}
