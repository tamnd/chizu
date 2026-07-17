package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// Doc 04 section 1: varints are unsigned LEB128, at most 10 bytes. Decoding
// additionally demands the minimal encoding, so that any accepted bytes
// re-encode to exactly the input; a padded varint is a malformed object,
// not an alternate spelling. That property is what the format-fuzz
// invariant stands on.

// AppendUvarint appends v as a minimal unsigned LEB128 varint.
func AppendUvarint(dst []byte, v uint64) []byte {
	return binary.AppendUvarint(dst, v)
}

// Uvarint decodes one varint from the front of data, returning the value
// and the bytes consumed. Truncated, oversized, and non-minimal encodings
// are errors.
func Uvarint(data []byte) (uint64, int, error) {
	v, n := binary.Uvarint(data)
	if n <= 0 {
		return 0, 0, errors.New("coldfmt: bad varint")
	}
	if n != uvarintLen(v) {
		return 0, 0, errors.New("coldfmt: non-minimal varint")
	}
	return v, n, nil
}

// uvarintLen is the minimal LEB128 length of v: one byte per started 7 bits.
func uvarintLen(v uint64) int {
	return max(1, (bits.Len64(v)+6)/7)
}

// AppendBytes appends a length-prefixed blob: len as a varint in u32 range,
// then the bytes, no terminator. Strings use the same shape.
func AppendBytes(dst, b []byte) []byte {
	if len(b) > math.MaxUint32 {
		panic("coldfmt: blob exceeds u32 length")
	}
	dst = AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// AppendString appends s with the AppendBytes shape.
func AppendString(dst []byte, s string) []byte {
	if len(s) > math.MaxUint32 {
		panic("coldfmt: string exceeds u32 length")
	}
	dst = AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// DecodeBytes decodes one length-prefixed blob from the front of data,
// returning a subslice of data and the total bytes consumed. A declared
// length past the u32 range or past the end of data is an error, never an
// allocation.
func DecodeBytes(data []byte) ([]byte, int, error) {
	l, n, err := Uvarint(data)
	if err != nil {
		return nil, 0, err
	}
	if l > math.MaxUint32 {
		return nil, 0, errors.New("coldfmt: blob length exceeds u32")
	}
	if l > uint64(len(data)-n) {
		return nil, 0, fmt.Errorf("coldfmt: blob length %d past end", l)
	}
	return data[n : n+int(l)], n + int(l), nil
}

// DecodeString is DecodeBytes with a string result.
func DecodeString(data []byte) (string, int, error) {
	b, n, err := DecodeBytes(data)
	return string(b), n, err
}

// Doc 04 section 1: sorted sequences store the first value as a varint,
// then successive differences as varints, strictly positive unless a field
// table says otherwise.

// AppendDeltas appends vals delta-coded. The input must be strictly
// increasing; anything else is a programmer error and panics.
func AppendDeltas(dst []byte, vals []uint64) []byte {
	for i, v := range vals {
		if i > 0 {
			if v <= vals[i-1] {
				panic("coldfmt: delta input not strictly increasing")
			}
			v -= vals[i-1]
		}
		dst = AppendUvarint(dst, v)
	}
	return dst
}

// DecodeDeltas decodes count delta-coded values from the front of data,
// returning the reconstructed sequence and the bytes consumed. Zero deltas
// and sequences that overflow u64 are errors.
func DecodeDeltas(data []byte, count int) ([]uint64, int, error) {
	vals := make([]uint64, 0, count)
	off := 0
	for i := range count {
		d, n, err := Uvarint(data[off:])
		if err != nil {
			return nil, 0, err
		}
		off += n
		if i > 0 {
			prev := vals[i-1]
			if d == 0 {
				return nil, 0, errors.New("coldfmt: zero delta")
			}
			if d > math.MaxUint64-prev {
				return nil, 0, errors.New("coldfmt: delta overflow")
			}
			d += prev
		}
		vals = append(vals, d)
	}
	return vals, off, nil
}
