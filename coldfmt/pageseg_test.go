package coldfmt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// testCorpus builds n deterministic rows with enough repeated prose that
// dictionary training has something to chew on.
func testCorpus(n int) []PageRow {
	rng := rand.New(rand.NewSource(2107))
	rows := make([]PageRow, n)
	fetch := uint64(1_700_000_000_000)
	for i := range rows {
		r := &rows[i]
		rng.Read(r.URLFP[:])
		rng.Read(r.SHA256[:])
		r.URL = fmt.Sprintf("https://example%d.test/path/%d", i%97, i)
		fetch += uint64(rng.Intn(3)) // repeats are legal in completion order
		r.FetchMS = fetch
		r.Status = 200
		if i%13 == 0 {
			r.Status = 404
		}
		r.Flags = PageRawStored
		if i%7 == 0 {
			r.Flags |= PageNearDup
		}
		r.Simhash = rng.Uint64()
		r.Lang = byte(i % 5)
		r.CanonFP = r.URLFP
		if i%11 == 0 {
			r.Flags |= PageAlias
			rng.Read(r.CanonFP[:])
		}
		r.Hdr = []byte("content-type: text/html\ncache-control: max-age=600")
		r.Title = fmt.Sprintf("Page %d about maps and search engines", i)
		r.Text = strings.Repeat(fmt.Sprintf("the quick brown fox %d jumps over the lazy dog and searches the web for maps. ", i), 20)
		for j := range i % 4 {
			var l Outlink
			rng.Read(l.Dst[:])
			l.Flags = LinkNofollow * byte(j%2)
			l.Anchor = fmt.Sprintf("anchor text %d-%d", i, j)
			r.Outlinks = append(r.Outlinks, l)
		}
		if i%3 == 0 {
			r.RawRef = RawRef{Seq: uint64(i), Off: uint32(i * 100), Len: 4096}
		}
		r.LawVer = 1
	}
	return rows
}

func sealCorpus(t *testing.T, rows []PageRow, blockTarget int) []byte {
	t.Helper()
	data, err := sealRows(rows, blockTarget)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sealRows(rows []PageRow, blockTarget int) ([]byte, error) {
	w := &PageSegmentWriter{Partition: 7, Epoch: 3, Seq: 42, Writer: 9, BlockTarget: blockTarget}
	for _, r := range rows {
		w.Add(r)
	}
	return w.Seal()
}

func TestPageSegmentRoundTrip(t *testing.T) {
	rows := testCorpus(500)
	data := sealCorpus(t, rows, 0)

	s, err := OpenPageSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Partition != 7 || s.Epoch != 3 || s.Seq != 42 || s.NRows != 500 || s.Header.Writer != 9 {
		t.Fatalf("meta: %+v", s)
	}
	if s.FirstFetchMS != rows[0].FetchMS || s.LastFetchMS != rows[499].FetchMS {
		t.Fatalf("time range %d..%d", s.FirstFetchMS, s.LastFetchMS)
	}
	got, err := s.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rows) {
		for i := range rows {
			if !reflect.DeepEqual(got[i], rows[i]) {
				t.Fatalf("row %d:\n got %+v\nwant %+v", i, got[i], rows[i])
			}
		}
		t.Fatal("rows differ")
	}
}

// A tiny block target forces every column into many blocks, which is the
// only way the fetch_ms per-block delta restart and the firstrow
// bookkeeping get exercised without a 100 MB fixture.
func TestPageSegmentMultiBlock(t *testing.T) {
	rows := testCorpus(300)
	data := sealCorpus(t, rows, 512)

	s, err := OpenPageSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := len(s.index[11]); n < 4 {
		t.Fatalf("text column has %d blocks, want several", n)
	}
	got, err := s.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rows) {
		t.Fatal("multi-block rows differ")
	}
}

func TestPageSegmentDictionaryPays(t *testing.T) {
	rows := testCorpus(500)
	data := sealCorpus(t, rows, 0)
	s, err := OpenPageSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.codec.dict) == 0 {
		t.Fatal("no dictionary trained on a 500-row prose corpus")
	}
	// The text column must actually compress: stored would be comp 0.
	b := s.index[11][0]
	if comp := s.data[b.offset+8]; comp != CompZstdDict {
		t.Fatalf("text block comp %d, want zstd-dict", comp)
	}
	if b.storedlen >= b.rawlen {
		t.Fatalf("text block did not shrink: %d stored, %d raw", b.storedlen, b.rawlen)
	}
}

func TestPageSegmentAnchorTruncation(t *testing.T) {
	rows := testCorpus(20)
	rows[3].Outlinks = []Outlink{{Anchor: strings.Repeat("a", 1000)}}
	data := sealCorpus(t, rows, 0)
	s, err := OpenPageSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got[3].Outlinks[0].Anchor) != MaxAnchorLen {
		t.Fatalf("anchor stored at %d bytes", len(got[3].Outlinks[0].Anchor))
	}
}

func TestPageSegmentBloom(t *testing.T) {
	rows := testCorpus(2000)
	data := sealCorpus(t, rows, 0)
	s, err := OpenPageSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range rows {
		if !s.MayContain(rows[i].URLFP) {
			t.Fatalf("bloom missed row %d", i)
		}
	}
	rng := rand.New(rand.NewSource(1))
	fps := 0
	for range 20000 {
		var fp [16]byte
		rng.Read(fp[:])
		if s.MayContain(fp) {
			fps++
		}
	}
	// 10 bits/row targets ~1%; anything past 3% means the bloom is broken.
	if fps > 600 {
		t.Fatalf("false positive rate %d/20000", fps)
	}
}

func TestPageSegmentRejectsDisorder(t *testing.T) {
	w := &PageSegmentWriter{}
	w.Add(PageRow{FetchMS: 10})
	w.Add(PageRow{FetchMS: 9})
	if _, err := w.Seal(); err == nil {
		t.Fatal("out-of-order rows sealed")
	}
	if _, err := (&PageSegmentWriter{}).Seal(); err == nil {
		t.Fatal("empty segment sealed")
	}
}

func TestPageSegmentRejectsCrossType(t *testing.T) {
	data := AppendHeader(nil, FormatGraphSeg, 1)
	data = append(data, 0xAB)
	data = binary.LittleEndian.AppendUint32(data, CRC(data[HeaderSize:]))
	data = AppendTail(data, uint64(len(data)), 0)
	if _, err := OpenPageSegment(data); err == nil {
		t.Fatal("graph-typed object opened as page segment")
	}
}

func TestPageSegmentFlipSweep(t *testing.T) {
	rows := testCorpus(60)
	good := sealCorpus(t, rows, 2048)
	s0, err := OpenPageSegment(good)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	footerOff, _, _ := ParseTail(good)
	var probe PageSegment
	probe.NRows = s0.NRows
	dictOff, dictLen, err := probe.parseFooter(good[footerOff : len(good)-TailSize])
	if err != nil {
		t.Fatal(err)
	}

	// Flipping any byte must never panic. Two regions may accept a flip:
	// the writer field (outside the header crc by design) and the
	// dictionary extent, the one region without its own crc; there the
	// zstd content checksum catches any flip that changes decompression,
	// so an accepted flip must decode to the exact original rows. Stride
	// keeps the sweep fast while still crossing every region.
	want, err := s0.Rows()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(good); i += 7 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		s, err := OpenPageSegment(bad)
		if err != nil {
			continue
		}
		got, err := s.Rows()
		s.Close()
		if err != nil {
			continue
		}
		writerField := i >= 24 && i < 32
		inDict := uint64(i) >= dictOff && uint64(i) < dictOff+uint64(dictLen)
		if !writerField && !inDict {
			t.Fatalf("flip at %d accepted silently", i)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("flip at %d changed decoded rows without an error", i)
		}
	}
}

func FuzzOpenPageSegment(f *testing.F) {
	for _, seed := range [][2]int{{30, 1024}, {8, 0}} {
		data, err := sealRows(testCorpus(seed[0]), seed[1])
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := OpenPageSegment(data)
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
	})
}
