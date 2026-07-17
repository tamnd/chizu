package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Docvalues band, doc 05 section 8: fixed 8 bytes per doc indexed by
// local docid, the only per-doc bytes the phase-1 scorer touches. The
// 8-byte budget is hard; a new signal must evict a nibble, not grow
// the record.

const docValueSize = 8

// DocValue is one doc's record. The doclen fields are log-bucketed
// norms: body gets a full byte, title and anchor share byte 7 as
// nibbles.
type DocValue struct {
	Quality      uint8
	Spam         uint8
	Lang         uint8
	Flags        uint8
	Date         uint16 // days since 2000-01-01, 0 unknown
	DoclenBody   uint8
	DoclenTitle  uint8 // nibble, 0-15
	DoclenAnchor uint8 // nibble, 0-15
}

// EncodeDocValues lays the records out docid-dense.
func EncodeDocValues(dvs []DocValue) ([]byte, error) {
	out := make([]byte, 0, len(dvs)*docValueSize)
	for i, dv := range dvs {
		if dv.DoclenTitle > 0x0F || dv.DoclenAnchor > 0x0F {
			return nil, fmt.Errorf("hotfmt: doclen nibble out of range at docid %d", i)
		}
		out = append(out, dv.Quality, dv.Spam, dv.Lang, dv.Flags)
		out = binary.LittleEndian.AppendUint16(out, dv.Date)
		out = append(out, dv.DoclenBody, dv.DoclenTitle|dv.DoclenAnchor<<4)
	}
	return out, nil
}

// DocValues is a read view over the band; At carries no bounds logic
// beyond the slice itself because the band length fixed the count.
type DocValues struct {
	data []byte
}

// OpenDocValues checks the band is a whole number of records for the
// stated doc count.
func OpenDocValues(data []byte, docCount uint32) (*DocValues, error) {
	if uint64(len(data)) != uint64(docCount)*docValueSize {
		return nil, errors.New("hotfmt: docvalues band length disagrees with doc count")
	}
	return &DocValues{data: data}, nil
}

func (d *DocValues) Len() int { return len(d.data) / docValueSize }

func (d *DocValues) At(docid uint32) DocValue {
	r := d.data[int(docid)*docValueSize:]
	return DocValue{
		Quality:      r[0],
		Spam:         r[1],
		Lang:         r[2],
		Flags:        r[3],
		Date:         binary.LittleEndian.Uint16(r[4:]),
		DoclenBody:   r[6],
		DoclenTitle:  r[7] & 0x0F,
		DoclenAnchor: r[7] >> 4,
	}
}
