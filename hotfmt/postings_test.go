package hotfmt

import (
	"bytes"
	"testing"
)

// fixturePostings builds n postings with a deterministic mix of small
// and huge docid gaps (exercising patches), tf spread, and field
// masks.
func fixturePostings(n int) []Posting {
	ps := make([]Posting, n)
	docid := uint32(0)
	for i := range ps {
		gap := uint32(i%7 + 1)
		if i%97 == 13 {
			gap = 1 << 20 // outlier gap, becomes a patch
		}
		docid += gap
		mask := uint8(1)
		if i%11 == 0 {
			mask = uint8(i%maxFieldMask + 1)
		}
		ps[i] = Posting{
			Docid:  docid,
			TF:     uint8(i%254 + 1),
			Mask:   mask,
			Impact: uint8(i % 251),
		}
	}
	return ps
}

// decodePostingsList walks every block of an encoded list.
func decodePostingsList(t *testing.T, data []byte) (docids []uint32, tfs, masks []uint8, blocks []PostingsBlock) {
	t.Helper()
	var db [PostingsBlockLen]uint32
	var tb, mb [PostingsBlockLen]uint8
	prev := int64(-1)
	pos := 0
	for pos < len(data) {
		b, n, err := DecodePostingsBlock(data[pos:], prev, db[:], tb[:], mb[:])
		if err != nil {
			t.Fatalf("decode block at %d: %v", pos, err)
		}
		docids = append(docids, db[:b.NEntries]...)
		tfs = append(tfs, tb[:b.NEntries]...)
		masks = append(masks, mb[:b.NEntries]...)
		blocks = append(blocks, b)
		prev = int64(b.LastDocid)
		pos += n
	}
	return docids, tfs, masks, blocks
}

func TestPostingsRoundTrip(t *testing.T) {
	for _, n := range []int{1, 4, 127, 128, 129, 256, 300, 1000} {
		ps := fixturePostings(n)
		data, l1, err := EncodePostings(ps)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		wantBlocks := (n + PostingsBlockLen - 1) / PostingsBlockLen
		if len(l1) != wantBlocks {
			t.Fatalf("n=%d: %d skip seeds, want %d", n, len(l1), wantBlocks)
		}
		docids, tfs, masks, blocks := decodePostingsList(t, data)
		if len(docids) != n {
			t.Fatalf("n=%d: decoded %d postings", n, len(docids))
		}
		for i, p := range ps {
			if docids[i] != p.Docid || tfs[i] != p.TF || masks[i] != p.Mask {
				t.Fatalf("n=%d posting %d: got (%d,%d,%d), want (%d,%d,%d)",
					n, i, docids[i], tfs[i], masks[i], p.Docid, p.TF, p.Mask)
			}
		}
		for bi, b := range blocks {
			if b.LastDocid != l1[bi].LastDocid || b.Impact != l1[bi].Impact {
				t.Fatalf("n=%d block %d header disagrees with skip seed", n, bi)
			}
			// Every block after the first must start exactly at its seed
			// offset; the first starts at 0.
			if bi == 0 && l1[bi].Off != 0 {
				t.Fatalf("n=%d: first block offset %d", n, l1[bi].Off)
			}
		}
		// A block must decode standalone from its skip offset with the
		// previous block's last docid, the traversal contract.
		for bi := range blocks {
			prev := int64(-1)
			if bi > 0 {
				prev = int64(blocks[bi-1].LastDocid)
			}
			var db [PostingsBlockLen]uint32
			var tb, mb [PostingsBlockLen]uint8
			if _, _, err := DecodePostingsBlock(data[l1[bi].Off:], prev, db[:], tb[:], mb[:]); err != nil {
				t.Fatalf("n=%d: block %d does not decode from skip offset: %v", n, bi, err)
			}
		}
	}
}

func TestPostingsBlockImpactIsMax(t *testing.T) {
	ps := fixturePostings(300)
	_, l1, err := EncodePostings(ps)
	if err != nil {
		t.Fatal(err)
	}
	for bi, e := range l1 {
		start := bi * PostingsBlockLen
		var want uint8
		for _, p := range ps[start:min(start+PostingsBlockLen, len(ps))] {
			want = max(want, p.Impact)
		}
		if e.Impact != want {
			t.Fatalf("block %d impact %d, want %d", bi, e.Impact, want)
		}
	}
}

func TestEncodePostingsRejects(t *testing.T) {
	cases := []struct {
		name string
		ps   []Posting
	}{
		{"empty", nil},
		{"duplicate docid", []Posting{{Docid: 5, TF: 1, Mask: 1}, {Docid: 5, TF: 1, Mask: 1}}},
		{"decreasing docid", []Posting{{Docid: 5, TF: 1, Mask: 1}, {Docid: 4, TF: 1, Mask: 1}}},
		{"tf zero", []Posting{{Docid: 5, Mask: 1}}},
		{"mask zero", []Posting{{Docid: 5, TF: 1}}},
		{"mask overflow", []Posting{{Docid: 5, TF: 1, Mask: 0x10}}},
	}
	for _, tc := range cases {
		if _, _, err := EncodePostings(tc.ps); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestDecodePostingsBlockRejectsCorruption(t *testing.T) {
	ps := fixturePostings(200)
	data, _, err := EncodePostings(ps)
	if err != nil {
		t.Fatal(err)
	}
	var db [PostingsBlockLen]uint32
	var tb, mb [PostingsBlockLen]uint8
	decode := func(d []byte) error {
		_, _, err := DecodePostingsBlock(d, -1, db[:], tb[:], mb[:])
		return err
	}
	if err := decode(data[:4]); err == nil {
		t.Error("truncated header accepted")
	}
	if err := decode(data[:20]); err == nil {
		t.Error("truncated body accepted")
	}
	bad := bytes.Clone(data)
	bad[0]++ // last_docid header
	if err := decode(bad); err == nil {
		t.Error("wrong last docid accepted")
	}
	bad = bytes.Clone(data)
	bad[6] |= 1 << 5 // unknown flag bit
	if err := decode(bad); err == nil {
		t.Error("unknown flag accepted")
	}
	bad = bytes.Clone(data)
	bad[7] = 0 // nentries
	if err := decode(bad); err == nil {
		t.Error("zero entries accepted")
	}
}

func FuzzDecodePostingsBlock(f *testing.F) {
	for _, n := range []int{1, 128, 300} {
		if data, _, err := EncodePostings(fixturePostings(n)); err == nil {
			f.Add(data)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var db [PostingsBlockLen]uint32
		var tb, mb [PostingsBlockLen]uint8
		b, n, err := DecodePostingsBlock(data, -1, db[:], tb[:], mb[:])
		if err != nil {
			return
		}
		if n > len(data) {
			t.Fatalf("decoder consumed %d of %d bytes", n, len(data))
		}
		// Semantic invariant: what decoded re-encodes to a block that
		// decodes identically. Byte identity is not demanded because the
		// decoder deliberately does not check the width choice.
		ps := make([]Posting, b.NEntries)
		for i := range ps {
			ps[i] = Posting{Docid: db[i], TF: tb[i], Mask: mb[i], Impact: b.Impact}
		}
		var out []byte
		if b.Tail {
			out, err = appendVbyteBlock(nil, ps, -1)
		} else {
			out, err = appendFORBlock(nil, ps, -1)
		}
		if err != nil {
			t.Fatalf("re-encode of decoded block: %v", err)
		}
		var db2 [PostingsBlockLen]uint32
		var tb2, mb2 [PostingsBlockLen]uint8
		b2, n2, err := DecodePostingsBlock(out, -1, db2[:], tb2[:], mb2[:])
		if err != nil {
			t.Fatalf("decode of re-encoded block: %v", err)
		}
		if n2 != len(out) {
			t.Fatalf("re-encoded block has %d slack bytes", len(out)-n2)
		}
		if b2.NEntries != b.NEntries || b2.LastDocid != b.LastDocid || b2.Impact != b.Impact {
			t.Fatal("re-encoded block header drifted")
		}
		for i := range b.NEntries {
			if db2[i] != db[i] || tb2[i] != tb[i] || mb2[i] != mb[i] {
				t.Fatalf("posting %d drifted through re-encode", i)
			}
		}
	})
}
