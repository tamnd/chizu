package hotfmt

import (
	"bytes"
	"reflect"
	"testing"
)

func TestMetaRoundTrip(t *testing.T) {
	want := fixtureMeta()
	data, err := EncodeMeta(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMeta(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v != %+v", got, want)
	}

	empty := &Meta{LawVer: 1}
	data, err = EncodeMeta(empty)
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseMeta(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.LawVer != 1 || len(got.Fields) != 0 || len(got.Lineage) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestMetaRejectsBadInput(t *testing.T) {
	m := fixtureMeta()
	m.Fields[1].ID = m.Fields[0].ID
	if _, err := EncodeMeta(m); err == nil {
		t.Fatal("duplicate field id accepted")
	}
	m = fixtureMeta()
	m.Fields[0].Name = ""
	if _, err := EncodeMeta(m); err == nil {
		t.Fatal("empty field name accepted")
	}

	good, err := EncodeMeta(fixtureMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseMeta(good[:len(good)-3]); err == nil {
		t.Fatal("truncated meta accepted")
	}
	if _, err := ParseMeta(append(bytes.Clone(good), 0)); err == nil {
		t.Fatal("trailing meta bytes accepted")
	}
}

func TestFieldStatsRoundTrip(t *testing.T) {
	want := fixtureStats()
	data, err := EncodeFieldStats(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseFieldStats(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v != %+v", got, want)
	}
}

func TestFieldStatsRejectsBadInput(t *testing.T) {
	s := fixtureStats()
	s.Fields[2].ID = s.Fields[1].ID
	if _, err := EncodeFieldStats(s); err == nil {
		t.Fatal("duplicate field id accepted")
	}

	good, err := EncodeFieldStats(fixtureStats())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFieldStats(good[:len(good)-1]); err == nil {
		t.Fatal("truncated fieldstats accepted")
	}
	if _, err := ParseFieldStats(append(bytes.Clone(good), 0)); err == nil {
		t.Fatal("trailing fieldstats bytes accepted")
	}
}

func FuzzParseMeta(f *testing.F) {
	for _, m := range []*Meta{fixtureMeta(), {LawVer: 1}} {
		data, err := EncodeMeta(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseMeta(data)
		if err != nil {
			return
		}
		out, err := EncodeMeta(m)
		if err != nil {
			t.Fatalf("accepted meta re-encode failed: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted meta did not re-encode to the input")
		}
	})
}

func FuzzParseFieldStats(f *testing.F) {
	for _, s := range []*FieldStats{fixtureStats(), {K1: 1.2}} {
		data, err := EncodeFieldStats(s)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseFieldStats(data)
		if err != nil {
			return
		}
		out, err := EncodeFieldStats(s)
		if err != nil {
			t.Fatalf("accepted fieldstats re-encode failed: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted fieldstats did not re-encode to the input")
		}
	})
}
