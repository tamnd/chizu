package hotfmt

import (
	"bytes"
	"math/bits"
	"reflect"
	"testing"
)

func fixtureRun(mask uint8) []FieldPositions {
	var fields []FieldPositions
	for f := range uint8(3) {
		if mask&positionFields&(1<<f) == 0 {
			continue
		}
		n := int(f)*3 + 2
		ps := make([]uint16, n)
		p := uint16(f) * 5
		for i := range ps {
			p += uint16(i%9 + 1)
			ps[i] = p
		}
		fields = append(fields, FieldPositions{Field: f, Positions: ps})
	}
	return fields
}

func TestPositionRunRoundTrip(t *testing.T) {
	for mask := uint8(1); mask <= maxFieldMask; mask++ {
		fields := fixtureRun(mask)
		data, err := AppendPositionRun(nil, mask, fields)
		if err != nil {
			t.Fatalf("mask=%#x: %v", mask, err)
		}
		if mask&positionFields == 0 && len(data) != 0 {
			t.Fatalf("mask=%#x: url-only posting wrote %d bytes", mask, len(data))
		}
		got, n, err := DecodePositionRun(data, mask)
		if err != nil {
			t.Fatalf("mask=%#x: %v", mask, err)
		}
		if n != len(data) {
			t.Fatalf("mask=%#x: consumed %d of %d bytes", mask, n, len(data))
		}
		if !reflect.DeepEqual(got, fields) {
			t.Fatalf("mask=%#x: run drifted: %+v vs %+v", mask, got, fields)
		}
	}
}

func TestPositionRunBoundaries(t *testing.T) {
	// The largest legal position must survive.
	fields := []FieldPositions{{Field: 0, Positions: []uint16{0, 1, maxPosition}}}
	data, err := AppendPositionRun(nil, 1, fields)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := DecodePositionRun(data, 1)
	if err != nil || !reflect.DeepEqual(got, fields) {
		t.Fatalf("max position run: %v %+v", err, got)
	}
}

func TestPositionRunRejects(t *testing.T) {
	cases := []struct {
		name   string
		mask   uint8
		fields []FieldPositions
	}{
		{"mask zero", 0, nil},
		{"mask overflow", 0x10, nil},
		{"missing field", 0b011, []FieldPositions{{Field: 0, Positions: []uint16{1}}}},
		{"extra field", 0b001, []FieldPositions{{Field: 0, Positions: []uint16{1}}, {Field: 1, Positions: []uint16{1}}}},
		{"field not in mask", 0b001, []FieldPositions{{Field: 1, Positions: []uint16{1}}}},
		{"fields out of order", 0b011, []FieldPositions{{Field: 1, Positions: []uint16{1}}, {Field: 0, Positions: []uint16{1}}}},
		{"empty positions", 0b001, []FieldPositions{{Field: 0, Positions: nil}}},
		{"positions not increasing", 0b001, []FieldPositions{{Field: 0, Positions: []uint16{5, 5}}}},
	}
	for _, tc := range cases {
		if _, err := AppendPositionRun(nil, tc.mask, tc.fields); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if _, _, err := DecodePositionRun([]byte{1}, 1); err == nil {
		t.Error("truncated run accepted")
	}
	// Non-minimal varint count.
	if _, _, err := DecodePositionRun([]byte{0x81, 0x00, 0x00}, 1); err == nil {
		t.Error("non-minimal varint accepted")
	}
}

func FuzzDecodePositionRun(f *testing.F) {
	for mask := uint8(1); mask <= maxFieldMask; mask += 3 {
		if data, err := AppendPositionRun(nil, mask, fixtureRun(mask)); err == nil {
			f.Add(data, mask)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, mask uint8) {
		fields, n, err := DecodePositionRun(data, mask)
		if err != nil {
			return
		}
		if len(fields) != bits.OnesCount8(mask&positionFields) {
			t.Fatal("decoded field count disagrees with mask")
		}
		out, err := AppendPositionRun(nil, mask, fields)
		if err != nil {
			t.Fatalf("re-encode of accepted run: %v", err)
		}
		if !bytes.Equal(out, data[:n]) {
			t.Fatal("accepted position run is not canonical")
		}
	})
}
