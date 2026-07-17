package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// GC tombstone page, doc 04 section 9: after a manifest supersedes its
// predecessor, the orphaned objects are not deleted inline; the GC writes
// their keys to a tombstone page and the sweeper deletes them after a
// grace period. Reason codes are the sweeper's table, opaque here, same
// stance as the fetch ledger's outcome byte.
//
// Small, read whole, flat shape: header, rows, trailing crc, no tail.

// maxTombstoneRows caps a declared row count; one page holds one GC
// pass's orphans, a few thousand keys at the very worst.
const maxTombstoneRows = 1 << 20

// maxTombstoneKeyLen bounds one bucket key; S3 caps keys at 1024 bytes.
const maxTombstoneKeyLen = 1024

// TombstoneRow marks one bucket object for deletion once EligibleMS has
// passed.
type TombstoneRow struct {
	Key        string
	Reason     byte
	EligibleMS uint64
}

// TombstonePage is one GC pass's deletion list.
type TombstonePage struct {
	Writer uint64
	Rows   []TombstoneRow
}

// EncodeTombstonePage lays out format 0x7A0A: nrows u32, then per row a
// length-prefixed key, reason u8, eligible_ms u64.
func EncodeTombstonePage(p *TombstonePage) ([]byte, error) {
	if len(p.Rows) > maxTombstoneRows {
		return nil, fmt.Errorf("coldfmt: %d rows exceeds tombstone cap", len(p.Rows))
	}
	out := AppendHeader(make([]byte, 0, HeaderSize+4+len(p.Rows)*32+4), FormatTombstone, p.Writer)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(p.Rows)))
	for _, r := range p.Rows {
		if len(r.Key) == 0 || len(r.Key) > maxTombstoneKeyLen {
			return nil, fmt.Errorf("coldfmt: tombstone key length %d", len(r.Key))
		}
		out = AppendString(out, r.Key)
		out = append(out, r.Reason)
		out = binary.LittleEndian.AppendUint64(out, r.EligibleMS)
	}
	return binary.LittleEndian.AppendUint32(out, CRC(out[HeaderSize:])), nil
}

// ParseTombstonePage verifies and decodes format 0x7A0A. The row area
// must be consumed exactly; trailing garbage is corruption, not slack.
func ParseTombstonePage(data []byte) (*TombstonePage, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	if h.Format != FormatTombstone {
		return nil, fmt.Errorf("coldfmt: format 0x%04X is not a tombstone page", h.Format)
	}
	if len(data) < HeaderSize+4+4 {
		return nil, errors.New("coldfmt: tombstone page too short")
	}
	body := data[HeaderSize : len(data)-4]
	if binary.LittleEndian.Uint32(data[len(data)-4:]) != CRC(body) {
		return nil, errors.New("coldfmt: tombstone page crc mismatch")
	}
	nrows := binary.LittleEndian.Uint32(body)
	if nrows > maxTombstoneRows {
		return nil, fmt.Errorf("coldfmt: %d rows exceeds tombstone cap", nrows)
	}
	p := &TombstonePage{Writer: h.Writer, Rows: make([]TombstoneRow, 0, min(nrows, 1<<14))}
	rest := body[4:]
	for range nrows {
		key, n, err := DecodeString(rest)
		if err != nil {
			return nil, err
		}
		if len(key) == 0 || len(key) > maxTombstoneKeyLen {
			return nil, fmt.Errorf("coldfmt: tombstone key length %d", len(key))
		}
		rest = rest[n:]
		if len(rest) < 9 {
			return nil, errors.New("coldfmt: tombstone row truncated")
		}
		p.Rows = append(p.Rows, TombstoneRow{
			Key:        key,
			Reason:     rest[0],
			EligibleMS: binary.LittleEndian.Uint64(rest[1:]),
		})
		rest = rest[9:]
	}
	if len(rest) != 0 {
		return nil, errors.New("coldfmt: tombstone page has trailing bytes")
	}
	return p, nil
}
