// Package hotfmt owns the bytes of .hot files, the immutable serving
// artifact of spec 2107 doc 05: one file per shard generation, bands
// laid out in access-pattern order and 4 KiB aligned so the resident
// bands mmap as one contiguous prefix and the big bands serve ranged
// preads. The package imports the stdlib plus klauspost/compress only;
// the shared 32-byte header family (doc 04 section 2) is small and
// frozen, so hotfmt carries its own copy of the codec, same as chain.
package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/bits"
)

const (
	magic         = "tamndchizu fmt01"
	fversion      = 1
	hotHeaderSize = 96
	tailSize      = 16
	formatHot     = 0x7A10

	// Every band starts on a 4 KiB boundary (doc 05 section 1), the
	// alignment O_DIRECT and the mmap plan both want.
	align = 4096
)

// Band ids, doc 05 section 1. The id is the logical name; the file
// position comes from bandOrder.
const (
	BandMeta       byte = 0
	BandDict       byte = 1
	BandPostings   byte = 2
	BandSkips      byte = 3
	BandPositions  byte = 4
	BandDocvalues  byte = 5
	BandDocband    byte = 6
	BandTombstones byte = 7
	BandFieldstats byte = 8

	numBands = 9
)

// bandOrder is the canonical file order: the mmap-resident bands
// (meta, fieldstats, dict, docvalues, tombstones) cluster at the front
// so residency is one contiguous prefix, and the three big pread bands
// sit behind them where alignment padding costs nothing.
var bandOrder = [numBands]byte{
	BandMeta, BandFieldstats, BandDict, BandDocvalues, BandTombstones,
	BandPostings, BandSkips, BandPositions, BandDocband,
}

// File kinds: a base carries a full shard, a delta carries recent docs
// against a base generation (doc 05 section 12). Zero is invalid on
// purpose so a zeroed header cannot pass for either.
const (
	KindBase  byte = 1
	KindDelta byte = 2
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// FileHeader is the parsed 96-byte header: the common 32 bytes, then
// the .hot extension, reserved padding to 92, and hcrc2 over the
// extension. The writer field stays the one crc-exempt region, same
// contract as every other format in the family.
type FileHeader struct {
	Shard        uint16
	Generation   uint64
	Kind         byte
	BaseGen      uint64
	DocCount     uint32
	TermCount    uint64
	TokenizerVer uint16
	QuantScale   float32
	Writer       uint64
}

func (h *FileHeader) validate() error {
	switch h.Kind {
	case KindBase:
		if h.BaseGen != 0 {
			return errors.New("hotfmt: base file with nonzero base_gen")
		}
	case KindDelta:
		if h.BaseGen >= h.Generation {
			return errors.New("hotfmt: delta base_gen not before generation")
		}
	default:
		return fmt.Errorf("hotfmt: unknown kind %d", h.Kind)
	}
	return nil
}

func appendFileHeader(dst []byte, h *FileHeader) []byte {
	dst = append(dst, magic...)
	dst = binary.LittleEndian.AppendUint16(dst, formatHot)
	dst = binary.LittleEndian.AppendUint16(dst, fversion)
	dst = binary.LittleEndian.AppendUint32(dst, crc(dst[len(dst)-20:]))
	dst = binary.LittleEndian.AppendUint64(dst, h.Writer)
	dst = binary.LittleEndian.AppendUint16(dst, h.Shard)
	dst = binary.LittleEndian.AppendUint64(dst, h.Generation)
	dst = append(dst, h.Kind)
	dst = binary.LittleEndian.AppendUint64(dst, h.BaseGen)
	dst = binary.LittleEndian.AppendUint32(dst, h.DocCount)
	dst = binary.LittleEndian.AppendUint64(dst, h.TermCount)
	dst = binary.LittleEndian.AppendUint16(dst, h.TokenizerVer)
	dst = binary.LittleEndian.AppendUint32(dst, f32bits(h.QuantScale))
	dst = append(dst, make([]byte, 92-69)...)
	return binary.LittleEndian.AppendUint32(dst, crc(dst[len(dst)-60:]))
}

func parseFileHeader(data []byte) (FileHeader, error) {
	if len(data) < hotHeaderSize {
		return FileHeader{}, errors.New("hotfmt: short header")
	}
	if string(data[:16]) != magic {
		return FileHeader{}, errors.New("hotfmt: bad magic")
	}
	if binary.LittleEndian.Uint32(data[20:]) != crc(data[:20]) {
		return FileHeader{}, errors.New("hotfmt: header crc mismatch")
	}
	if f := binary.LittleEndian.Uint16(data[16:]); f != formatHot {
		return FileHeader{}, fmt.Errorf("hotfmt: format 0x%04X is not a hot shard", f)
	}
	if v := binary.LittleEndian.Uint16(data[18:]); v == 0 || v > fversion {
		return FileHeader{}, fmt.Errorf("hotfmt: unknown fversion %d", v)
	}
	if binary.LittleEndian.Uint32(data[92:]) != crc(data[32:92]) {
		return FileHeader{}, errors.New("hotfmt: header extension crc mismatch")
	}
	for _, b := range data[69:92] {
		if b != 0 {
			return FileHeader{}, errors.New("hotfmt: nonzero reserved header bytes")
		}
	}
	h := FileHeader{
		Writer:       binary.LittleEndian.Uint64(data[24:]),
		Shard:        binary.LittleEndian.Uint16(data[32:]),
		Generation:   binary.LittleEndian.Uint64(data[34:]),
		Kind:         data[42],
		BaseGen:      binary.LittleEndian.Uint64(data[43:]),
		DocCount:     binary.LittleEndian.Uint32(data[51:]),
		TermCount:    binary.LittleEndian.Uint64(data[55:]),
		TokenizerVer: binary.LittleEndian.Uint16(data[63:]),
		QuantScale:   f32from(binary.LittleEndian.Uint32(data[65:])),
	}
	if err := h.validate(); err != nil {
		return FileHeader{}, err
	}
	return h, nil
}

// alignUp rounds n up to the next band boundary.
func alignUp(n uint64) uint64 {
	return (n + align - 1) &^ uint64(align-1)
}

// maxU40 bounds the 5-byte offsets the dictionary and skips bands use;
// 1 TB of band is far past the reference shard's largest band.
const maxU40 = 1<<40 - 1

func appendU40(dst []byte, v uint64) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32))
}

func u40(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 | uint64(b[4])<<32
}

func appendU48(dst []byte, v uint64) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40))
}

func u48(b []byte) uint64 {
	return u40(b) | uint64(b[5])<<40
}

// appendUvarint and uvarint mirror the doc 04 varint law: unsigned
// LEB128, minimal encoding demanded at decode so accepted bytes
// re-encode to exactly the input.
func appendUvarint(dst []byte, v uint64) []byte {
	return binary.AppendUvarint(dst, v)
}

func uvarint(data []byte) (uint64, int, error) {
	v, n := binary.Uvarint(data)
	if n <= 0 {
		return 0, 0, errors.New("hotfmt: bad varint")
	}
	if n != max(1, (bits.Len64(v)+6)/7) {
		return 0, 0, errors.New("hotfmt: non-minimal varint")
	}
	return v, n, nil
}
