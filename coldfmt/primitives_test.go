package coldfmt

import (
	"bytes"
	"math"
	"slices"
	"testing"
)

func TestUvarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 20, 1<<32 - 1, 1 << 62, math.MaxUint64} {
		data := AppendUvarint(nil, v)
		got, n, err := Uvarint(data)
		if err != nil || got != v || n != len(data) {
			t.Fatalf("v=%d got=%d n=%d err=%v", v, got, n, err)
		}
	}
}

func TestUvarintRejectsNonMinimal(t *testing.T) {
	// 0x80 0x00 spells zero in two bytes; only 0x00 is the format's zero.
	cases := [][]byte{
		{0x80, 0x00},
		{0xFF, 0x00},
		{0x81, 0x80, 0x00},
		{},
		{0x80}, // truncated
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}, // 11 bytes
	}
	for _, c := range cases {
		if _, _, err := Uvarint(c); err == nil {
			t.Fatalf("accepted %x", c)
		}
	}
}

func TestBytesAndStringRoundTrip(t *testing.T) {
	data := AppendString(nil, "地図")
	data = AppendBytes(data, []byte{0, 1, 2})
	data = AppendString(data, "")

	s, n, err := DecodeString(data)
	if err != nil || s != "地図" {
		t.Fatalf("s=%q err=%v", s, err)
	}
	b, m, err := DecodeBytes(data[n:])
	if err != nil || !bytes.Equal(b, []byte{0, 1, 2}) {
		t.Fatalf("b=%x err=%v", b, err)
	}
	empty, _, err := DecodeString(data[n+m:])
	if err != nil || empty != "" {
		t.Fatalf("empty=%q err=%v", empty, err)
	}
}

func TestDecodeBytesRejectsOversizedLength(t *testing.T) {
	// Declared length far past the end of data must error without allocating.
	data := AppendUvarint(nil, 1<<40)
	data = append(data, "short"...)
	if _, _, err := DecodeBytes(data); err == nil {
		t.Fatal("oversized declared length accepted")
	}
}

func TestDeltasRoundTrip(t *testing.T) {
	vals := []uint64{3, 4, 100, 1 << 33, 1<<33 + 1}
	data := AppendDeltas(nil, vals)
	got, n, err := DecodeDeltas(data, len(vals))
	if err != nil || n != len(data) || !slices.Equal(got, vals) {
		t.Fatalf("got=%v n=%d err=%v", got, n, err)
	}
	if got, _, err := DecodeDeltas(nil, 0); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestDeltasRejectBadInput(t *testing.T) {
	// A zero delta spells a duplicate, which a sorted sequence cannot hold.
	zero := AppendUvarint(AppendUvarint(nil, 5), 0)
	if _, _, err := DecodeDeltas(zero, 2); err == nil {
		t.Fatal("zero delta accepted")
	}
	// A delta wrapping past MaxUint64 must reject, not wrap.
	wrap := AppendUvarint(AppendUvarint(nil, math.MaxUint64), 1)
	if _, _, err := DecodeDeltas(wrap, 2); err == nil {
		t.Fatal("overflowing delta accepted")
	}
	if _, _, err := DecodeDeltas(AppendUvarint(nil, 7), 2); err == nil {
		t.Fatal("truncated sequence accepted")
	}
}

func TestAppendDeltasPanicsOnUnsortedInput(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic on non-increasing input")
		}
	}()
	AppendDeltas(nil, []uint64{5, 5})
}

func FuzzUvarint(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add(AppendUvarint(nil, math.MaxUint64))
	f.Add([]byte{0x80, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		v, n, err := Uvarint(data)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendUvarint(nil, v), data[:n]) {
			t.Fatal("accepted varint does not re-encode to input")
		}
	})
}

func FuzzDecodeBytes(f *testing.F) {
	f.Add(AppendBytes(nil, []byte("chizu")))
	f.Add([]byte{0x05})
	f.Fuzz(func(t *testing.T, data []byte) {
		b, n, err := DecodeBytes(data)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendBytes(nil, b), data[:n]) {
			t.Fatal("accepted blob does not re-encode to input")
		}
	})
}

func FuzzDecodeDeltas(f *testing.F) {
	f.Add(AppendDeltas(nil, []uint64{3, 4, 100}), uint16(3))
	f.Add([]byte{0x00, 0x00}, uint16(2))
	f.Fuzz(func(t *testing.T, data []byte, count uint16) {
		vals, n, err := DecodeDeltas(data, int(count)%1024)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendDeltas(nil, vals), data[:n]) {
			t.Fatal("accepted deltas do not re-encode to input")
		}
	})
}
