package hotfmt

import (
	"errors"
	"fmt"
	"math/bits"
)

// Positions band, doc 05 section 7: per term, one run per posting in
// docid order; inside a run, per field present in the posting's mask,
// a count then position deltas, all minimal varints capped at u16.
// URL tokens (mask bit 3) carry no positions, so a run covers only
// the mask's low three bits.

// positionFields masks the fields that carry positions: body, title,
// anchor. Bit 3 (url) never does.
const positionFields = 0b0111

const maxPosition = 1<<16 - 1

// FieldPositions is one field's positions inside a run, strictly
// increasing, each at most maxPosition.
type FieldPositions struct {
	Field     uint8
	Positions []uint16
}

// AppendPositionRun encodes one posting's position run. fields must
// cover exactly the bits of mask & positionFields, in ascending field
// order; a posting whose mask has no position-bearing bits appends
// nothing.
func AppendPositionRun(dst []byte, mask uint8, fields []FieldPositions) ([]byte, error) {
	if mask == 0 || mask > maxFieldMask {
		return nil, fmt.Errorf("hotfmt: fieldmask 0x%02X", mask)
	}
	want := mask & positionFields
	if len(fields) != bits.OnesCount8(want) {
		return nil, errors.New("hotfmt: run fields disagree with mask")
	}
	for i, f := range fields {
		if f.Field > 7 || want&(1<<f.Field) == 0 {
			return nil, fmt.Errorf("hotfmt: field %d not in mask", f.Field)
		}
		if i > 0 && f.Field <= fields[i-1].Field {
			return nil, errors.New("hotfmt: run fields out of order")
		}
		if len(f.Positions) == 0 {
			return nil, errors.New("hotfmt: field present with no positions")
		}
		dst = appendUvarint(dst, uint64(len(f.Positions)))
		prev := -1
		for _, p := range f.Positions {
			if int(p) <= prev {
				return nil, errors.New("hotfmt: positions not strictly increasing")
			}
			dst = appendUvarint(dst, uint64(int(p)-prev-1))
			prev = int(p)
		}
	}
	return dst, nil
}

// DecodePositionRun decodes one run for a posting with the given
// mask, returning the fields and the bytes consumed. The caller knows
// the mask from the postings block, so the run stores no mask of its
// own.
func DecodePositionRun(data []byte, mask uint8) ([]FieldPositions, int, error) {
	if mask == 0 || mask > maxFieldMask {
		return nil, 0, fmt.Errorf("hotfmt: fieldmask 0x%02X", mask)
	}
	var fields []FieldPositions
	pos := 0
	for i := range 3 {
		f := uint8(i)
		if mask&positionFields&(1<<f) == 0 {
			continue
		}
		count, n, err := uvarint(data[pos:])
		if err != nil {
			return nil, 0, err
		}
		pos += n
		if count == 0 || count > maxPosition+1 {
			return nil, 0, fmt.Errorf("hotfmt: position count %d", count)
		}
		ps := make([]uint16, count)
		prev := -1
		for i := range ps {
			d, n, err := uvarint(data[pos:])
			if err != nil {
				return nil, 0, err
			}
			pos += n
			// Bound the delta before the addition so a huge varint cannot
			// wrap the arithmetic.
			if d > maxPosition {
				return nil, 0, errors.New("hotfmt: position exceeds u16")
			}
			p := int64(prev) + int64(d) + 1
			if p > maxPosition {
				return nil, 0, errors.New("hotfmt: position exceeds u16")
			}
			ps[i] = uint16(p)
			prev = int(p)
		}
		fields = append(fields, FieldPositions{Field: f, Positions: ps})
	}
	return fields, pos, nil
}
