package coldfmt

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func tombstoneCorpus(n int) *TombstonePage {
	p := &TombstonePage{Writer: 11}
	for i := range n {
		p.Rows = append(p.Rows, TombstoneRow{
			Key:        fmt.Sprintf("cold/page/p0042/%016d.cold", i),
			Reason:     byte(i % 5),
			EligibleMS: 1_700_000_000_000 + uint64(i)*3600_000,
		})
	}
	return p
}

func TestTombstonePageRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 500} {
		want := tombstoneCorpus(n)
		data, err := EncodeTombstonePage(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseTombstonePage(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Rows) == 0 {
			got.Rows = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("n=%d round trip differs", n)
		}
	}
}

func TestTombstonePageRejectsBadInput(t *testing.T) {
	if _, err := EncodeTombstonePage(&TombstonePage{Rows: []TombstoneRow{{Key: ""}}}); err == nil {
		t.Fatal("empty key accepted")
	}
	long := strings.Repeat("k", maxTombstoneKeyLen+1)
	if _, err := EncodeTombstonePage(&TombstonePage{Rows: []TombstoneRow{{Key: long}}}); err == nil {
		t.Fatal("oversized key accepted")
	}

	good, err := EncodeTombstonePage(tombstoneCorpus(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTombstonePage(good[:len(good)-6]); err == nil {
		t.Fatal("truncated page accepted")
	}
}

func TestTombstonePageFlipSweep(t *testing.T) {
	want := tombstoneCorpus(20)
	good, err := EncodeTombstonePage(want)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(good); i += 3 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		got, err := ParseTombstonePage(bad)
		if err != nil {
			continue
		}
		if i < 24 || i >= 32 {
			t.Fatalf("flip at %d accepted silently", i)
		}
		got.Writer = want.Writer
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("flip at %d changed the page beyond the writer field", i)
		}
	}
}

func FuzzParseTombstonePage(f *testing.F) {
	for _, n := range []int{0, 4} {
		data, err := EncodeTombstonePage(tombstoneCorpus(n))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseTombstonePage(data)
		if err != nil {
			return
		}
		out, err := EncodeTombstonePage(p)
		if err != nil {
			t.Fatalf("accepted page re-encode failed: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted page did not re-encode to the input")
		}
	})
}
