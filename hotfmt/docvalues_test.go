package hotfmt

import (
	"bytes"
	"reflect"
	"testing"
)

func fixtureDocValues(n int) []DocValue {
	dvs := make([]DocValue, n)
	for i := range dvs {
		dvs[i] = DocValue{
			Quality:      uint8(255 - i%256),
			Spam:         uint8(i % 7),
			Lang:         uint8(i % 40),
			Flags:        uint8(i % 4),
			Date:         uint16(9000 + i%1000),
			DoclenBody:   uint8(i % 200),
			DoclenTitle:  uint8(i % 16),
			DoclenAnchor: uint8((i * 3) % 16),
		}
	}
	return dvs
}

func TestDocValuesRoundTrip(t *testing.T) {
	dvs := fixtureDocValues(300)
	band, err := EncodeDocValues(dvs)
	if err != nil {
		t.Fatal(err)
	}
	if len(band) != 300*docValueSize {
		t.Fatalf("band is %d bytes", len(band))
	}
	d, err := OpenDocValues(band, 300)
	if err != nil {
		t.Fatal(err)
	}
	if d.Len() != 300 {
		t.Fatalf("Len = %d", d.Len())
	}
	for i, want := range dvs {
		if got := d.At(uint32(i)); !reflect.DeepEqual(got, want) {
			t.Fatalf("docid %d: %+v, want %+v", i, got, want)
		}
	}
}

func TestDocValuesRejects(t *testing.T) {
	if _, err := EncodeDocValues([]DocValue{{DoclenTitle: 16}}); err == nil {
		t.Error("title nibble overflow accepted")
	}
	if _, err := EncodeDocValues([]DocValue{{DoclenAnchor: 16}}); err == nil {
		t.Error("anchor nibble overflow accepted")
	}
	band, err := EncodeDocValues(fixtureDocValues(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDocValues(band, 9); err == nil {
		t.Error("doc count skew accepted")
	}
	if _, err := OpenDocValues(band[:len(band)-1], 10); err == nil {
		t.Error("truncated band accepted")
	}
}

func FuzzOpenDocValues(f *testing.F) {
	if band, err := EncodeDocValues(fixtureDocValues(10)); err == nil {
		f.Add(band)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data)%docValueSize != 0 {
			return
		}
		n := uint32(len(data) / docValueSize)
		d, err := OpenDocValues(data, n)
		if err != nil {
			return
		}
		dvs := make([]DocValue, n)
		for i := range dvs {
			dvs[i] = d.At(uint32(i))
		}
		out, err := EncodeDocValues(dvs)
		if err != nil {
			t.Fatalf("re-encode of accepted docvalues: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted docvalues band is not canonical")
		}
	})
}
