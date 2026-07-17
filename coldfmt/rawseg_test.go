package coldfmt

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func rawCorpus(n int) ([][16]byte, [][]byte) {
	rng := rand.New(rand.NewSource(2107))
	fps := make([][16]byte, n)
	htmls := make([][]byte, n)
	for i := range fps {
		rng.Read(fps[i][:])
		htmls[i] = fmt.Appendf(nil, "<html><head><title>Page %d</title></head><body>%s</body></html>",
			i, strings.Repeat(fmt.Sprintf("<p>paragraph %d about maps and search</p>", i), 30))
	}
	return fps, htmls
}

func sealRaw(fps [][16]byte, htmls [][]byte, blockTarget int) ([]byte, error) {
	w := &RawSegmentWriter{Partition: 7, Epoch: 3, Seq: 42, Writer: 9, BlockTarget: blockTarget}
	for i := range fps {
		w.Add(fps[i], htmls[i])
	}
	return w.Seal()
}

func TestRawSegmentRoundTrip(t *testing.T) {
	fps, htmls := rawCorpus(200)
	data, err := sealRaw(fps, htmls, 4096)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenRawSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Partition != 7 || s.Epoch != 3 || s.Seq != 42 || s.Header.Writer != 9 {
		t.Fatalf("meta: %+v", s)
	}
	if len(s.Docs()) != 200 {
		t.Fatalf("%d docs", len(s.Docs()))
	}
	for i, d := range s.Docs() {
		if d.URLFP != fps[i] {
			t.Fatalf("doc %d fingerprint mismatch", i)
		}
		got, err := s.HTML(d)
		if err != nil {
			t.Fatalf("doc %d: %v", i, err)
		}
		if !bytes.Equal(got, htmls[i]) {
			t.Fatalf("doc %d html differs", i)
		}
	}
}

func TestRawSegmentGet(t *testing.T) {
	fps, htmls := rawCorpus(50)
	data, err := sealRaw(fps, htmls, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenRawSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok, err := s.Get(fps[37])
	if err != nil || !ok || !bytes.Equal(got, htmls[37]) {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	var missing [16]byte
	missing[0] = 0xFF
	if _, ok, _ := s.Get(missing); ok {
		t.Fatal("found a fingerprint that was never added")
	}
}

func TestRawSegmentRejectsEmpty(t *testing.T) {
	if _, err := (&RawSegmentWriter{}).Seal(); err == nil {
		t.Fatal("empty raw segment sealed")
	}
}

func TestRawSegmentFlipSweep(t *testing.T) {
	fps, htmls := rawCorpus(40)
	good, err := sealRaw(fps, htmls, 8192)
	if err != nil {
		t.Fatal(err)
	}
	s0, err := OpenRawSegment(good)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	footerOff, _, _ := ParseTail(good)
	var probe RawSegment
	dictOff, dictLen, err := probe.parseFooter(good[footerOff : len(good)-TailSize])
	if err != nil {
		t.Fatal(err)
	}

	// Same contract as the page segment sweep: accepted flips only in the
	// writer field or the dict extent, and there the decode must be
	// byte-identical to the original.
	for i := 0; i < len(good); i += 7 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		s, err := OpenRawSegment(bad)
		if err != nil {
			continue
		}
		clean := true
		for di, d := range s.Docs() {
			got, err := s.HTML(d)
			if err != nil {
				clean = false
				break
			}
			if !bytes.Equal(got, htmls[di]) {
				t.Fatalf("flip at %d changed doc %d without an error", i, di)
			}
		}
		s.Close()
		if !clean {
			continue
		}
		writerField := i >= 24 && i < 32
		inDict := uint64(i) >= dictOff && uint64(i) < dictOff+uint64(dictLen)
		if !writerField && !inDict {
			t.Fatalf("flip at %d accepted silently", i)
		}
	}
}

func FuzzOpenRawSegment(f *testing.F) {
	fps, htmls := rawCorpus(20)
	for _, target := range []int{0, 2048} {
		data, err := sealRaw(fps, htmls, target)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := OpenRawSegment(data)
		if err != nil {
			return
		}
		defer s.Close()
		for _, d := range s.Docs() {
			if _, err := s.HTML(d); err != nil {
				return
			}
		}
	})
}
