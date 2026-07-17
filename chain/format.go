// Package chain is the CAS-appended total order every chizu plane agrees
// through: one immutable batch object per sequence slot under
// <prefix>chain/<seq16>, records inside. Semantics are spec 2107 doc 02
// section 4; the bytes are doc 04 section 8 (format 0x7A02).
package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Doc 04 section 2: every chizu object opens with the same 32-byte header.
const (
	magic       = "tamndchizu fmt01"
	formatRoot  = 0x7A01
	formatBatch = 0x7A02
	fversion    = 1
	headerSize  = 32
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// appendHeader writes the 32-byte common header.
func appendHeader(dst []byte, format uint16, writer uint64) []byte {
	dst = append(dst, magic...)
	dst = binary.LittleEndian.AppendUint16(dst, format)
	dst = binary.LittleEndian.AppendUint16(dst, fversion)
	dst = binary.LittleEndian.AppendUint32(dst, crc(dst[len(dst)-20:]))
	return binary.LittleEndian.AppendUint64(dst, writer)
}

// openObject verifies the common header and the trailing body crc, both of
// which every chain-plane object carries, and hands back the writer and the
// payload between them.
func openObject(data []byte, format uint16) (writer uint64, payload []byte, err error) {
	if len(data) < headerSize+4 {
		return 0, nil, errors.New("chain: object too short")
	}
	if string(data[:16]) != magic {
		return 0, nil, errors.New("chain: bad magic")
	}
	if f := binary.LittleEndian.Uint16(data[16:]); f != format {
		return 0, nil, fmt.Errorf("chain: format 0x%04X, want 0x%04X", f, format)
	}
	if v := binary.LittleEndian.Uint16(data[18:]); v == 0 || v > fversion {
		return 0, nil, fmt.Errorf("chain: unknown fversion %d", v)
	}
	if binary.LittleEndian.Uint32(data[20:]) != crc(data[:20]) {
		return 0, nil, errors.New("chain: header crc mismatch")
	}
	payload = data[headerSize : len(data)-4]
	if binary.LittleEndian.Uint32(data[len(data)-4:]) != crc(payload) {
		return 0, nil, errors.New("chain: body crc mismatch")
	}
	return binary.LittleEndian.Uint64(data[24:]), payload, nil
}

// Batch is one chain slot: everything a writer had pending, appended in one
// CAS-create. The sequence number lives in the object key, not in the bytes,
// so a batch that loses its slot retries at the next one unchanged.
type Batch struct {
	// Writer and Incarnation identify the appending process; BatchID is
	// writer-unique. Together they let a writer whose PUT timed out re-read
	// the slot and learn whether its own batch is the one that landed.
	Writer      uint64
	Incarnation uint32
	BatchID     uint64
	Records     []Record
}

// Encode lays the batch out per doc 04 section 8: common header, batch_id,
// incarnation, nrecords, records (rlen u16, kind u8, body), crc over
// everything after the header.
func (b *Batch) Encode() ([]byte, error) {
	out := appendHeader(make([]byte, 0, 128), formatBatch, b.Writer)
	out = binary.LittleEndian.AppendUint64(out, b.BatchID)
	out = binary.LittleEndian.AppendUint32(out, b.Incarnation)
	if len(b.Records) > 0xFFFF {
		return nil, fmt.Errorf("chain: %d records exceed the u16 batch limit", len(b.Records))
	}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(b.Records)))
	for _, r := range b.Records {
		body := r.encodeBody(nil)
		if len(body) > 0xFFFF {
			return nil, fmt.Errorf("chain: kind-%d record body is %d bytes, over the u16 limit", r.Kind(), len(body))
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(len(body)))
		out = append(out, r.Kind())
		out = append(out, body...)
	}
	out = binary.LittleEndian.AppendUint32(out, crc(out[headerSize:]))
	return out, nil
}

// DecodeBatch parses and fully verifies one batch object. Any magic, format,
// version, crc, length, or trailing-byte problem is an error, never a partial
// result: a slot either decodes completely or does not exist to the caller.
func DecodeBatch(data []byte) (*Batch, error) {
	writer, payload, err := openObject(data, formatBatch)
	if err != nil {
		return nil, err
	}
	r := &reader{b: payload}
	b := &Batch{
		Writer:      writer,
		BatchID:     r.u64(),
		Incarnation: r.u32(),
	}
	n := int(r.u16())
	for range n {
		rlen := int(r.u16())
		kind := r.u8()
		body := r.take(rlen)
		if r.err != nil {
			return nil, r.err
		}
		rec, err := decodeRecord(kind, body)
		if err != nil {
			return nil, err
		}
		b.Records = append(b.Records, rec)
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.b) != 0 {
		return nil, fmt.Errorf("chain: %d trailing bytes after %d records", len(r.b), n)
	}
	return b, nil
}

var errTruncated = errors.New("chain: truncated")

// reader is an error-sticky cursor: after the first short read every further
// accessor returns zero values and the error survives for the caller to check.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.err = errTruncated
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() byte {
	b := r.take(1)
	if r.err != nil {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *reader) str() string {
	n := int(r.u16())
	return string(r.take(n))
}

func appendStr(dst []byte, s string) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(s)))
	return append(dst, s...)
}
