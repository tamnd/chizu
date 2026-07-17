// Package coldfmt owns the bytes chizu writes at rest: the common 32-byte
// header every object opens with, the 16-byte tail every multi-part object
// ends with, and the encoding primitives every structure in spec 2107
// doc 04 is built from. The package imports the stdlib only; the import
// boundary script enforces that edge.
package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Doc 04 section 2: one magic family for every object chizu ever writes.
const (
	Magic      = "tamndchizu fmt01"
	FVersion   = 1
	HeaderSize = 32
	TailSize   = 16
)

// The format code table, doc 04 section 2.
const (
	FormatRoot      uint16 = 0x7A01
	FormatBatch     uint16 = 0x7A02
	FormatCkpt      uint16 = 0x7A03
	FormatPageSeg   uint16 = 0x7A04
	FormatRawHTML   uint16 = 0x7A05
	FormatFrontier  uint16 = 0x7A06
	FormatGraphSeg  uint16 = 0x7A07
	FormatDocmap    uint16 = 0x7A08
	FormatManifest  uint16 = 0x7A09
	FormatTombstone uint16 = 0x7A0A
	FormatHotShard  uint16 = 0x7A10
	FormatEliasFano uint16 = 0x7A11 // reserved, doc 01 road not taken
	FormatWebGraph  uint16 = 0x7A12 // reserved, doc 01 road not taken
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC is crc32c (Castagnoli) over exactly the given bytes; every header,
// block, and footer in the format carries one.
func CRC(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// Header is the parsed common header. FVersion is the version the writer
// wrote, which a reader may need to pick a decoder; Writer is a node id.
type Header struct {
	Format   uint16
	FVersion uint16
	Writer   uint64
}

// AppendHeader writes the 32-byte common header at the current FVersion.
// The header crc covers bytes 0..19; the writer field rides outside it and
// is vouched for by each object's own trailing or footer crc.
func AppendHeader(dst []byte, format uint16, writer uint64) []byte {
	dst = append(dst, Magic...)
	dst = binary.LittleEndian.AppendUint16(dst, format)
	dst = binary.LittleEndian.AppendUint16(dst, FVersion)
	dst = binary.LittleEndian.AppendUint32(dst, CRC(dst[len(dst)-20:]))
	return binary.LittleEndian.AppendUint64(dst, writer)
}

// ParseHeader verifies the magic, header crc, and version range, and hands
// back the fields. Checking the format code against what the caller expects
// stays with the caller: only it knows whether a cross-typed object is an
// error or a probe.
func ParseHeader(data []byte) (Header, error) {
	if len(data) < HeaderSize {
		return Header{}, errors.New("coldfmt: short header")
	}
	if string(data[:16]) != Magic {
		return Header{}, errors.New("coldfmt: bad magic")
	}
	if binary.LittleEndian.Uint32(data[20:]) != CRC(data[:20]) {
		return Header{}, errors.New("coldfmt: header crc mismatch")
	}
	h := Header{
		Format:   binary.LittleEndian.Uint16(data[16:]),
		FVersion: binary.LittleEndian.Uint16(data[18:]),
		Writer:   binary.LittleEndian.Uint64(data[24:]),
	}
	if h.FVersion == 0 || h.FVersion > FVersion {
		return Header{}, fmt.Errorf("coldfmt: unknown fversion %d", h.FVersion)
	}
	return h, nil
}

// AppendTail writes the standard 16-byte tail that ends every multi-part
// object: footer_off u64, footer_len u32, tcrc u32 over the previous 12
// bytes. The open sequence reads this tail first, then the footer it names.
func AppendTail(dst []byte, footerOff uint64, footerLen uint32) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, footerOff)
	dst = binary.LittleEndian.AppendUint32(dst, footerLen)
	return binary.LittleEndian.AppendUint32(dst, CRC(dst[len(dst)-12:]))
}

// ParseTail verifies the tail occupying the last 16 bytes of data, so a
// caller can pass either the 16 bytes of a ranged read or a whole object.
// Whether footer_off and footer_len land inside the object is the caller's
// check; only it knows the object size.
func ParseTail(data []byte) (footerOff uint64, footerLen uint32, err error) {
	if len(data) < TailSize {
		return 0, 0, errors.New("coldfmt: short tail")
	}
	t := data[len(data)-TailSize:]
	if binary.LittleEndian.Uint32(t[12:]) != CRC(t[:12]) {
		return 0, 0, errors.New("coldfmt: tail crc mismatch")
	}
	return binary.LittleEndian.Uint64(t), binary.LittleEndian.Uint32(t[8:]), nil
}
