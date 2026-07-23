package hotfmt

import (
	"bytes"
	"reflect"
	"testing"
)

func fixtureSkips(df uint32) ([]SkipL1, []uint64) {
	nb, _ := SkipCounts(df)
	l1 := make([]SkipL1, nb)
	pos := make([]uint64, nb)
	for i := range l1 {
		l1[i] = SkipL1{
			LastDocid: uint32(i+1) * 1000,
			Impact:    uint8(i * 7 % 256),
			Off:       uint64(i) * 220,
		}
		pos[i] = uint64(i) * 900
	}
	return l1, pos
}

func TestSkipsRoundTrip(t *testing.T) {
	for _, df := range []uint32{1, 127, 128, 129, 128 * 32, 128*32 + 1, 128 * 33, 500000} {
		l1, pos := fixtureSkips(df)
		region, err := EncodeSkips(nil, l1, pos)
		if err != nil {
			t.Fatalf("df=%d: %v", df, err)
		}
		if len(region) != SkipRegionSize(df) {
			t.Fatalf("df=%d: region is %d bytes, formula says %d", df, len(region), SkipRegionSize(df))
		}
		gl1, gpos, gl2, err := ParseSkips(region, df)
		if err != nil {
			t.Fatalf("df=%d: %v", df, err)
		}
		if !reflect.DeepEqual(gl1, l1) || !reflect.DeepEqual(gpos, pos) {
			t.Fatalf("df=%d: L1 or positions drifted", df)
		}
		_, nl2 := SkipCounts(df)
		if len(gl2) != nl2 {
			t.Fatalf("df=%d: %d L2 entries, want %d", df, len(gl2), nl2)
		}
	}
}

func TestSkipsRejects(t *testing.T) {
	l1, pos := fixtureSkips(1000)
	if _, err := EncodeSkips(nil, nil, nil); err == nil {
		t.Error("empty arrays accepted")
	}
	if _, err := EncodeSkips(nil, l1, pos[:len(pos)-1]); err == nil {
		t.Error("mismatched arrays accepted")
	}
	bad := append([]SkipL1(nil), l1...)
	bad[3].LastDocid = bad[2].LastDocid
	if _, err := EncodeSkips(nil, bad, pos); err == nil {
		t.Error("non-increasing last docid accepted")
	}

	region, err := EncodeSkips(nil, l1, pos)
	if err != nil {
		t.Fatal(err)
	}
	// df 1001 shares df 1000's block count, so the region is legitimately
	// identical; a df in the next block bucket must be rejected.
	if _, _, _, err := ParseSkips(region, 2000); err == nil {
		t.Error("wrong df accepted")
	}
	if _, _, _, err := ParseSkips(region[:len(region)-1], 1000); err == nil {
		t.Error("truncated region accepted")
	}
	corrupt := bytes.Clone(region)
	corrupt[len(corrupt)-1] = 1 // L2 padding byte
	if _, _, _, err := ParseSkips(corrupt, 1000); err == nil {
		t.Error("nonzero L2 padding accepted")
	}
	corrupt = bytes.Clone(region)
	corrupt[len(corrupt)-8] ^= 0xFF // L2 impact
	if _, _, _, err := ParseSkips(corrupt, 1000); err == nil {
		t.Error("L2 disagreeing with L1 accepted")
	}
}

func FuzzParseSkips(f *testing.F) {
	for _, df := range []uint32{1, 129, 128 * 33} {
		l1, pos := fixtureSkips(df)
		if region, err := EncodeSkips(nil, l1, pos); err == nil {
			f.Add(region, df)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, df uint32) {
		l1, pos, _, err := ParseSkips(data, df)
		if err != nil {
			return
		}
		out, err := EncodeSkips(nil, l1, pos)
		if err != nil {
			t.Fatalf("re-encode of accepted skip region: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted skip region is not canonical")
		}
	})
}

func TestSkipRawAccessors(t *testing.T) {
	for _, df := range []uint32{1, 127, 128, 129, 128*32 + 1, 500000} {
		l1, pos := fixtureSkips(df)
		region, err := EncodeSkips(nil, l1, pos)
		if err != nil {
			t.Fatalf("df=%d: %v", df, err)
		}
		var bound uint8
		for i := range l1 {
			if got := SkipL1At(region, i); got != l1[i] {
				t.Fatalf("df=%d entry %d: %+v, want %+v", df, i, got, l1[i])
			}
			bound = max(bound, l1[i].Impact)
		}
		if got := SkipBound(region, df); got != bound {
			t.Fatalf("df=%d: bound %d, want %d", df, got, bound)
		}
	}
}
