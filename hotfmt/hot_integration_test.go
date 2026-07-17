package hotfmt

import (
	"bytes"
	"reflect"
	"testing"
)

// The integration fixture builds a real tiny shard: three terms wired
// through dictionary, postings, skips, and positions, plus docvalues
// and doc band, assembled with EncodeFile and read back through Open
// the way the serve plane will.

func integrationPostings(term string) []Posting {
	switch term {
	case "banana": // df 2, inlined in the dictionary
		return []Posting{
			{Docid: 3, TF: 1, Mask: 2},
			{Docid: 7, TF: 4, Mask: 5},
		}
	case "apple": // df 5, one vbyte tail block
		ps := make([]Posting, 5)
		for i := range ps {
			ps[i] = Posting{Docid: uint32(i * 10), TF: uint8(i + 1), Mask: uint8(i%3 + 1), Impact: uint8(40 + i)}
		}
		return ps
	case "the": // df 300, two FOR blocks plus a tail
		ps := make([]Posting, 300)
		for i := range ps {
			ps[i] = Posting{
				Docid:  uint32(i + i/3),
				TF:     uint8(i%9 + 1),
				Mask:   uint8(i%7 + 1),
				Impact: uint8(i % 100),
			}
		}
		return ps
	}
	return nil
}

// integrationRun derives a posting's position run from its docid and
// mask, so the reader can regenerate the expectation.
func integrationRun(p Posting) []FieldPositions {
	var fields []FieldPositions
	for f := range uint8(3) {
		if p.Mask&positionFields&(1<<f) == 0 {
			continue
		}
		base := uint16(p.Docid%100) + uint16(f)*7
		fields = append(fields, FieldPositions{
			Field:     f,
			Positions: []uint16{base, base + 2, base + 5},
		})
	}
	return fields
}

type termLayout struct {
	entry   DictEntry
	l1      []SkipL1
	posOffs []uint64
	runOffs []uint64 // per posting, absolute into the positions band
}

// buildIntegrationShard assembles the whole file and returns it with
// the per-term layout the assertions need.
func buildIntegrationShard(t *testing.T) ([]byte, map[string]*termLayout) {
	t.Helper()
	const docCount = 500
	terms := []string{"apple", "banana", "the"} // dictionary order
	layouts := make(map[string]*termLayout)

	var postingsBand, skipsBand, positionsBand []byte
	for _, term := range terms {
		ps := integrationPostings(term)
		lay := &termLayout{}
		layouts[term] = lay
		if len(ps) <= 4 {
			for _, p := range ps {
				lay.entry.Inline = append(lay.entry.Inline, InlinePosting{Docid: p.Docid, TF: p.TF, Mask: p.Mask})
			}
			lay.entry.DF = uint32(len(ps))
			continue
		}
		var cf uint64
		for _, p := range ps {
			cf += uint64(p.TF)
		}
		enc, l1, err := EncodePostings(ps)
		if err != nil {
			t.Fatalf("%s: %v", term, err)
		}
		lay.l1 = l1
		for _, p := range ps {
			lay.runOffs = append(lay.runOffs, uint64(len(positionsBand)))
			positionsBand, err = AppendPositionRun(positionsBand, p.Mask, integrationRun(p))
			if err != nil {
				t.Fatalf("%s: %v", term, err)
			}
		}
		for bi := range l1 {
			lay.posOffs = append(lay.posOffs, lay.runOffs[bi*PostingsBlockLen])
		}
		lay.entry = DictEntry{
			DF:          uint32(len(ps)),
			CF:          cf,
			PostingsOff: uint64(len(postingsBand)),
			PostingsLen: uint32(len(enc)),
			SkipOff:     uint64(len(skipsBand)),
		}
		postingsBand = append(postingsBand, enc...)
		skipsBand, err = EncodeSkips(skipsBand, l1, lay.posOffs)
		if err != nil {
			t.Fatalf("%s: %v", term, err)
		}
	}

	var dw DictWriter
	for _, term := range terms {
		if err := dw.Add([]byte(term), &layouts[term].entry); err != nil {
			t.Fatalf("dict %s: %v", term, err)
		}
	}
	dictBand, err := dw.Seal()
	if err != nil {
		t.Fatal(err)
	}
	dvBand, err := EncodeDocValues(fixtureDocValues(docCount))
	if err != nil {
		t.Fatal(err)
	}
	docBand, err := buildDocBand(docCount)
	if err != nil {
		t.Fatal(err)
	}

	h := fixtureHeader()
	h.DocCount = docCount
	h.TermCount = uint64(len(terms))
	meta := fixtureMeta()
	meta.DocCount = docCount
	data, err := EncodeFile(h, meta, fixtureStats(), map[byte][]byte{
		BandDict:      dictBand,
		BandPostings:  postingsBand,
		BandSkips:     skipsBand,
		BandPositions: positionsBand,
		BandDocvalues: dvBand,
		BandDocband:   docBand,
	}, fixtureProv())
	if err != nil {
		t.Fatal(err)
	}
	return data, layouts
}

func TestShardEndToEnd(t *testing.T) {
	data, layouts := buildIntegrationShard(t)
	s, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyBands(); err != nil {
		t.Fatal(err)
	}

	dictBand, err := s.ReadBand(BandDict)
	if err != nil {
		t.Fatal(err)
	}
	d, err := OpenDict(dictBand)
	if err != nil {
		t.Fatal(err)
	}

	// The inlined term never touches another band.
	e, ok, err := d.Lookup([]byte("banana"))
	if err != nil || !ok {
		t.Fatalf("Lookup(banana): ok=%v err=%v", ok, err)
	}
	wantInline := integrationPostings("banana")
	for i, p := range e.Inline {
		if p.Docid != wantInline[i].Docid || p.TF != wantInline[i].TF || p.Mask != wantInline[i].Mask {
			t.Fatalf("inline posting %d drifted", i)
		}
	}

	postingsBand, err := s.ReadBand(BandPostings)
	if err != nil {
		t.Fatal(err)
	}
	skipsBand, err := s.ReadBand(BandSkips)
	if err != nil {
		t.Fatal(err)
	}
	positionsBand, err := s.ReadBand(BandPositions)
	if err != nil {
		t.Fatal(err)
	}

	for _, term := range []string{"apple", "the"} {
		lay := layouts[term]
		want := integrationPostings(term)
		e, ok, err := d.Lookup([]byte(term))
		if err != nil || !ok {
			t.Fatalf("Lookup(%s): ok=%v err=%v", term, ok, err)
		}
		if e.DF != uint32(len(want)) {
			t.Fatalf("%s: df %d", term, e.DF)
		}

		termBytes := postingsBand[e.PostingsOff : e.PostingsOff+uint64(e.PostingsLen)]
		docids, tfs, masks, _ := decodePostingsList(t, termBytes)
		for i, p := range want {
			if docids[i] != p.Docid || tfs[i] != p.TF || masks[i] != p.Mask {
				t.Fatalf("%s posting %d drifted", term, i)
			}
		}

		region := skipsBand[e.SkipOff : e.SkipOff+uint64(SkipRegionSize(e.DF))]
		l1, posOffs, _, err := ParseSkips(region, e.DF)
		if err != nil {
			t.Fatalf("%s skips: %v", term, err)
		}
		if !reflect.DeepEqual(l1, lay.l1) || !reflect.DeepEqual(posOffs, lay.posOffs) {
			t.Fatalf("%s skip region drifted", term)
		}

		// Traversal contract: every block decodes standalone from its
		// skip offset, and its position runs decode from the parallel
		// offset, one run per posting in the block.
		for bi := range l1 {
			prev := int64(-1)
			if bi > 0 {
				prev = int64(l1[bi-1].LastDocid)
			}
			var db [PostingsBlockLen]uint32
			var tb, mb [PostingsBlockLen]uint8
			b, _, err := DecodePostingsBlock(termBytes[l1[bi].Off:], prev, db[:], tb[:], mb[:])
			if err != nil {
				t.Fatalf("%s block %d from skip: %v", term, bi, err)
			}
			pos := posOffs[bi]
			for i := range b.NEntries {
				p := want[bi*PostingsBlockLen+i]
				fields, n, err := DecodePositionRun(positionsBand[pos:], mb[i])
				if err != nil {
					t.Fatalf("%s block %d run %d: %v", term, bi, i, err)
				}
				if !reflect.DeepEqual(fields, integrationRun(p)) {
					t.Fatalf("%s docid %d positions drifted", term, p.Docid)
				}
				pos += uint64(n)
			}
		}
	}

	dvBand, err := s.ReadBand(BandDocvalues)
	if err != nil {
		t.Fatal(err)
	}
	dv, err := OpenDocValues(dvBand, s.Header.DocCount)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dv.At(123), fixtureDocValues(500)[123]; got != want {
		t.Fatalf("docvalues drifted: %+v vs %+v", got, want)
	}

	docBand, err := s.ReadBand(BandDocband)
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenDocBand(docBand)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec, err := db.Doc(321)
	if err != nil {
		t.Fatal(err)
	}
	if want := fixtureDocRecord(321); !bytes.Equal(rec.URL, want.URL) || !bytes.Equal(rec.Snippet, want.Snippet) {
		t.Fatal("doc record drifted")
	}
}
