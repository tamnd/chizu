package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Manifests, doc 04 section 9: one manifest family per (.cold family,
// partition), complete not delta, superseding all predecessors. The newest
// valid manifest is the truth about what exists; segments referenced by no
// valid manifest are garbage awaiting the sweep.
//
// A manifest is small and read whole, so it takes the chain-plane shape:
// common header, flat table, trailing crc, no tail.

// Manifest family codes.
const (
	FamilyPage byte = iota + 1
	FamilyRaw
	FamilyFrontier
	FamilyGraph
	FamilyDocmap
)

// maxManifestSegs caps a declared live-segment count; steady state at
// 100B-page scale is ~1,200 per partition.
const maxManifestSegs = 1 << 20

// manifestSegSize is one live-segment entry: seq u64, bytes u64, nrows
// u64, first_ms u64, last_ms u64, flags u16.
const manifestSegSize = 42

// ManifestSeg is one live segment.
type ManifestSeg struct {
	Seq     uint64
	Bytes   uint64
	NRows   uint64
	FirstMS uint64
	LastMS  uint64
	Flags   uint16
}

// Manifest is one complete live-set listing.
type Manifest struct {
	Family    byte
	Partition uint16
	Epoch     uint32
	ManSeq    uint64
	Watermark uint64
	Writer    uint64
	Segs      []ManifestSeg
}

// EncodeManifest lays out format 0x7A09. Segments must be listed in
// strictly increasing seq order; that is the format's sort contract.
func EncodeManifest(m *Manifest) ([]byte, error) {
	if m.Family < FamilyPage || m.Family > FamilyDocmap {
		return nil, fmt.Errorf("coldfmt: unknown manifest family %d", m.Family)
	}
	if len(m.Segs) > maxManifestSegs {
		return nil, fmt.Errorf("coldfmt: %d segments exceeds manifest cap", len(m.Segs))
	}
	for i := 1; i < len(m.Segs); i++ {
		if m.Segs[i].Seq <= m.Segs[i-1].Seq {
			return nil, errors.New("coldfmt: manifest segments not strictly increasing by seq")
		}
	}
	out := AppendHeader(make([]byte, 0, HeaderSize+27+len(m.Segs)*manifestSegSize+4), FormatManifest, m.Writer)
	out = append(out, m.Family)
	out = binary.LittleEndian.AppendUint16(out, m.Partition)
	out = binary.LittleEndian.AppendUint32(out, m.Epoch)
	out = binary.LittleEndian.AppendUint64(out, m.ManSeq)
	out = binary.LittleEndian.AppendUint64(out, m.Watermark)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(m.Segs)))
	for _, s := range m.Segs {
		out = binary.LittleEndian.AppendUint64(out, s.Seq)
		out = binary.LittleEndian.AppendUint64(out, s.Bytes)
		out = binary.LittleEndian.AppendUint64(out, s.NRows)
		out = binary.LittleEndian.AppendUint64(out, s.FirstMS)
		out = binary.LittleEndian.AppendUint64(out, s.LastMS)
		out = binary.LittleEndian.AppendUint16(out, s.Flags)
	}
	return binary.LittleEndian.AppendUint32(out, CRC(out[HeaderSize:])), nil
}

// ParseManifest verifies and decodes format 0x7A09.
func ParseManifest(data []byte) (*Manifest, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatManifest {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a manifest", h.Format)
	}
	if len(data) < HeaderSize+27+4 {
		return nil, errors.New("coldfmt: manifest too short")
	}
	body := data[HeaderSize : len(data)-4]
	if binary.LittleEndian.Uint32(data[len(data)-4:]) != CRC(body) {
		return nil, errors.New("coldfmt: manifest crc mismatch")
	}
	m := &Manifest{
		Family:    body[0],
		Partition: binary.LittleEndian.Uint16(body[1:]),
		Epoch:     binary.LittleEndian.Uint32(body[3:]),
		ManSeq:    binary.LittleEndian.Uint64(body[7:]),
		Watermark: binary.LittleEndian.Uint64(body[15:]),
		Writer:    h.Writer,
	}
	if m.Family < FamilyPage || m.Family > FamilyDocmap {
		return nil, fmt.Errorf("coldfmt: unknown manifest family %d", m.Family)
	}
	nsegs := binary.LittleEndian.Uint32(body[23:])
	if nsegs > maxManifestSegs {
		return nil, fmt.Errorf("coldfmt: %d segments exceeds manifest cap", nsegs)
	}
	if uint64(len(body)-27) != uint64(nsegs)*manifestSegSize {
		return nil, errors.New("coldfmt: manifest segment table length mismatch")
	}
	m.Segs = make([]ManifestSeg, nsegs)
	pos := 27
	for i := range m.Segs {
		m.Segs[i] = ManifestSeg{
			Seq:     binary.LittleEndian.Uint64(body[pos:]),
			Bytes:   binary.LittleEndian.Uint64(body[pos+8:]),
			NRows:   binary.LittleEndian.Uint64(body[pos+16:]),
			FirstMS: binary.LittleEndian.Uint64(body[pos+24:]),
			LastMS:  binary.LittleEndian.Uint64(body[pos+32:]),
			Flags:   binary.LittleEndian.Uint16(body[pos+40:]),
		}
		pos += manifestSegSize
		if i > 0 && m.Segs[i].Seq <= m.Segs[i-1].Seq {
			return nil, errors.New("coldfmt: manifest segments not strictly increasing by seq")
		}
	}
	return m, nil
}

// SelectManifest applies the epoch-validity rule (doc 04 section 9, A-I2
// instantiated): a manifest is valid only if its epoch held the
// partition's lease at some chain position at or after the previous VALID
// manifest's watermark. heldAtOrAfter answers exactly that from the chain's
// lease history; every reader folds the same history, so every reader
// picks the same winner, and a zombie's manifest is invalid by the same
// rule that makes its segments orphans. Returns nil when no candidate is
// valid.
//
// All candidates must belong to one (family, partition); mixing is a
// caller bug, reported as an error.
func SelectManifest(mans []*Manifest, heldAtOrAfter func(epoch uint32, pos uint64) bool) (*Manifest, error) {
	if len(mans) == 0 {
		return nil, nil
	}
	for _, m := range mans[1:] {
		if m.Family != mans[0].Family || m.Partition != mans[0].Partition {
			return nil, errors.New("coldfmt: manifests from more than one family or partition")
		}
	}
	ordered := make([]*Manifest, len(mans))
	copy(ordered, mans)
	sort.SliceStable(ordered, func(a, b int) bool { return ordered[a].ManSeq < ordered[b].ManSeq })
	var winner *Manifest
	watermark := uint64(0)
	for _, m := range ordered {
		if heldAtOrAfter(m.Epoch, watermark) {
			winner = m
			watermark = m.Watermark
		}
	}
	return winner, nil
}
