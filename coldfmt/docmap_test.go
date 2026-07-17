package coldfmt

import (
	"bytes"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func docmapCorpus(n int) []DocmapRow {
	rng := rand.New(rand.NewSource(2107))
	rows := make([]DocmapRow, n)
	for i := range rows {
		rng.Read(rows[i].URLFP[:])
		rng.Read(rows[i].SHA256[:])
		rows[i].DocID = uint64(i) + 1000
		rows[i].Quality = byte(i % 200)
		rows[i].Flags = byte(i % 4)
		rows[i].FetchMS = 1_700_000_000_000 + uint64(i)
	}
	sort.Slice(rows, func(a, b int) bool {
		return bytes.Compare(rows[a].URLFP[:], rows[b].URLFP[:]) < 0
	})
	return rows
}

func sealDocmap(rows []DocmapRow, blockTarget int) ([]byte, error) {
	w := &DocmapWriter{Shard: 7, Epoch: 3, Seq: 42, Writer: 9, BlockTarget: blockTarget}
	for _, r := range rows {
		w.Add(r)
	}
	return w.Seal()
}

func TestDocmapRoundTrip(t *testing.T) {
	rows := docmapCorpus(1000)
	data, err := sealDocmap(rows, 66*100)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenDocmapSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Shard != 7 || s.Epoch != 3 || s.Seq != 42 || s.NRows != 1000 || s.Header.Writer != 9 {
		t.Fatalf("meta: %+v", s)
	}
	if len(s.blocks) != 10 {
		t.Fatalf("%d blocks, want 10", len(s.blocks))
	}
	got, err := s.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rows) {
		t.Fatal("rows differ")
	}
}

func TestDocmapFind(t *testing.T) {
	rows := docmapCorpus(1000)
	data, err := sealDocmap(rows, 66*64)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenDocmapSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, i := range []int{0, 1, 63, 64, 500, 998, 999} {
		got, ok, err := s.Find(rows[i].URLFP)
		if err != nil || !ok {
			t.Fatalf("row %d: ok=%v err=%v", i, ok, err)
		}
		if got != rows[i] {
			t.Fatalf("row %d: got %+v want %+v", i, got, rows[i])
		}
	}
	var missing [16]byte // sorts before every random fingerprint in practice
	if _, ok, _ := s.Find(missing); ok {
		t.Fatal("found a fingerprint that was never added")
	}
	missing[0] = 0xFF
	for i := 1; i < 16; i++ {
		missing[i] = 0xFF
	}
	if _, ok, _ := s.Find(missing); ok {
		t.Fatal("found the all-FF fingerprint")
	}
}

func TestDocmapRejectsDisorder(t *testing.T) {
	rows := docmapCorpus(10)
	rows[3], rows[4] = rows[4], rows[3]
	if _, err := sealDocmap(rows, 0); err == nil {
		t.Fatal("unsorted docmap sealed")
	}
	dup := docmapCorpus(10)
	dup[5].URLFP = dup[4].URLFP
	if _, err := sealDocmap(dup, 0); err == nil {
		t.Fatal("duplicate fingerprint sealed")
	}
	if _, err := (&DocmapWriter{}).Seal(); err == nil {
		t.Fatal("empty docmap sealed")
	}
}

func TestDocmapFlipSweep(t *testing.T) {
	rows := docmapCorpus(120)
	good, err := sealDocmap(rows, 66*32)
	if err != nil {
		t.Fatal(err)
	}
	want, err := func() ([]DocmapRow, error) {
		s, err := OpenDocmapSegment(good)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return s.Rows()
	}()
	if err != nil {
		t.Fatal(err)
	}
	// No dictionary in this format either: writer field only.
	for i := 0; i < len(good); i += 5 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		s, err := OpenDocmapSegment(bad)
		if err != nil {
			continue
		}
		got, err := s.Rows()
		s.Close()
		if err != nil {
			continue
		}
		if !(i >= 24 && i < 32) {
			t.Fatalf("flip at %d accepted silently", i)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("flip at %d changed decoded rows without an error", i)
		}
	}
}

func FuzzOpenDocmapSegment(f *testing.F) {
	for _, target := range []int{0, 66 * 8} {
		data, err := sealDocmap(docmapCorpus(30), target)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := OpenDocmapSegment(data)
		if err != nil {
			return
		}
		defer s.Close()
		rows, err := s.Rows()
		if err != nil {
			return
		}
		if uint64(len(rows)) != s.NRows {
			t.Fatal("accepted segment with wrong row count")
		}
		// Every row an accepted segment holds must be findable.
		got, ok, err := s.Find(rows[0].URLFP)
		if err != nil || !ok || got != rows[0] {
			t.Fatal("accepted segment cannot find its own first row")
		}
	})
}
