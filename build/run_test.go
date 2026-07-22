package build

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func testRecs() []Rec {
	return []Rec{
		{Term: []byte("alpha"), Docid: 0, TF: 3, Mask: 0b01, Pos: [NumFields][]uint16{{0, 4, 9}, nil}},
		{Term: []byte("alpha"), Docid: 7, TF: 1, Mask: 0b10, Pos: [NumFields][]uint16{nil, {2}}},
		{Term: []byte("beta"), Docid: 7, TF: 2, Mask: 0b11, Pos: [NumFields][]uint16{{100}, {0}}},
		{Term: []byte("日本"), Docid: 9, TF: 1, Mask: 0b01, Pos: [NumFields][]uint16{{65535}, nil}},
	}
}

func TestRunRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run-0000.czrn")
	w, err := NewRunWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	want := testRecs()
	for i := range want {
		if err := w.Add(&want[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := OpenRun(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var rec Rec
	for i := range want {
		if err := r.Next(&rec); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if string(rec.Term) != string(want[i].Term) || rec.Docid != want[i].Docid ||
			rec.TF != want[i].TF || rec.Mask != want[i].Mask {
			t.Fatalf("record %d: got %+v want %+v", i, rec, want[i])
		}
		for f := range NumFields {
			if len(rec.Pos[f]) != len(want[i].Pos[f]) {
				t.Fatalf("record %d field %d: %v want %v", i, f, rec.Pos[f], want[i].Pos[f])
			}
			for j := range rec.Pos[f] {
				if rec.Pos[f][j] != want[i].Pos[f][j] {
					t.Fatalf("record %d field %d: %v want %v", i, f, rec.Pos[f], want[i].Pos[f])
				}
			}
		}
	}
	if err := r.Next(&rec); err != io.EOF {
		t.Fatalf("want io.EOF after last record, got %v", err)
	}
}

func TestOpenRunRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(path, []byte("not a run file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRun(path); err == nil {
		t.Fatal("garbage header accepted")
	}
}
