package build

import (
	"bufio"
	"bytes"
	"testing"
)

// FuzzReadRec holds the run codec's parser contract on arbitrary bytes:
// no panic, bounded output, and whatever decodes re-encodes to the
// bytes it was decoded from (the codec has one canonical spelling only
// when deltas fit minimal uvarints, so the check is decode-encode-decode
// stability, not byte identity).
func FuzzReadRec(f *testing.F) {
	var seed []byte
	for _, r := range testRecs() {
		seed = appendRec(seed, &r)
	}
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{5, 'a', 'l', 'p', 'h', 'a', 0, 1, 1, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		br := bufio.NewReader(bytes.NewReader(data))
		var recs []Rec
		var rec Rec
		for {
			if err := readRec(br, &rec); err != nil {
				break
			}
			if len(rec.Term) == 0 || len(rec.Term) > maxRecTerm {
				t.Fatalf("term length %d out of bounds", len(rec.Term))
			}
			cp := Rec{Term: bytes.Clone(rec.Term), Docid: rec.Docid, TF: rec.TF, Mask: rec.Mask}
			for fi := range NumFields {
				cp.Pos[fi] = append([]uint16(nil), rec.Pos[fi]...)
			}
			recs = append(recs, cp)
			if len(recs) > 1<<12 {
				break
			}
		}
		// Round-trip what decoded: encode, decode again, compare.
		var enc []byte
		for i := range recs {
			enc = appendRec(enc, &recs[i])
		}
		br2 := bufio.NewReader(bytes.NewReader(enc))
		for i := range recs {
			var got Rec
			if err := readRec(br2, &got); err != nil {
				t.Fatalf("re-decode record %d: %v", i, err)
			}
			if !bytes.Equal(got.Term, recs[i].Term) || got.Docid != recs[i].Docid ||
				got.TF != recs[i].TF || got.Mask != recs[i].Mask {
				t.Fatalf("record %d unstable: %+v vs %+v", i, got, recs[i])
			}
			for fi := range NumFields {
				if len(got.Pos[fi]) != len(recs[i].Pos[fi]) {
					t.Fatalf("record %d field %d unstable", i, fi)
				}
				for j := range got.Pos[fi] {
					if got.Pos[fi][j] != recs[i].Pos[fi][j] {
						t.Fatalf("record %d field %d position %d unstable", i, fi, j)
					}
				}
			}
		}
	})
}
