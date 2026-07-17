package coldfmt

import (
	"bytes"
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

func graphCorpus(n int) ([]uint64, [][]uint64) {
	rng := rand.New(rand.NewSource(2107))
	srcs := make([]uint64, n)
	dsts := make([][]uint64, n)
	next := uint64(1_000_000)
	for i := range srcs {
		next += uint64(rng.Intn(50) + 1)
		srcs[i] = next
		deg := rng.Intn(30)
		if i%17 == 0 {
			deg = 0 // pages with no outlinks still own their node id
		}
		seen := map[uint64]bool{}
		for range deg {
			d := uint64(rng.Intn(5_000_000))
			if !seen[d] {
				seen[d] = true
				dsts[i] = append(dsts[i], d)
			}
		}
		slices.Sort(dsts[i])
	}
	return srcs, dsts
}

func sealGraph(srcs []uint64, dsts [][]uint64, blockTarget int) ([]byte, error) {
	w := &GraphSegmentWriter{Generation: 5, Part: 3, Writer: 9, BlockTarget: blockTarget}
	for i := range srcs {
		if err := w.Add(srcs[i], dsts[i]); err != nil {
			return nil, err
		}
	}
	return w.Seal()
}

func walkAll(t *testing.T, s *GraphSegment) ([]uint64, [][]uint64) {
	t.Helper()
	var srcs []uint64
	var dsts [][]uint64
	if err := s.Walk(func(src uint64, d []uint64) error {
		srcs = append(srcs, src)
		dsts = append(dsts, slices.Clone(d))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return srcs, dsts
}

func TestGraphSegmentRoundTrip(t *testing.T) {
	srcs, dsts := graphCorpus(2000)
	data, err := sealGraph(srcs, dsts, 4096)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenGraphSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Generation != 5 || s.Part != 3 || s.Header.Writer != 9 {
		t.Fatalf("meta: %+v", s)
	}
	if s.FirstSrc != srcs[0] || s.LastSrc != srcs[len(srcs)-1] || s.NSrc != 2000 {
		t.Fatalf("range: %+v", s)
	}
	if len(s.blocks) < 4 {
		t.Fatalf("%d blocks, want several", len(s.blocks))
	}
	gs, gd := walkAll(t, s)
	if !reflect.DeepEqual(gs, srcs) {
		t.Fatal("sources differ")
	}
	for i := range dsts {
		want := dsts[i]
		if want == nil {
			want = []uint64{}
		}
		if !slices.Equal(gd[i], want) {
			t.Fatalf("source %d destinations differ", srcs[i])
		}
	}
}

func TestGraphSegmentU40Bounds(t *testing.T) {
	w := &GraphSegmentWriter{}
	if err := w.Add(MaxNodeID+1, nil); err == nil {
		t.Fatal("u40 overflow source accepted")
	}
	if err := w.Add(5, []uint64{MaxNodeID + 1}); err == nil {
		t.Fatal("u40 overflow destination accepted")
	}
	if err := w.Add(MaxNodeID, []uint64{0, MaxNodeID}); err != nil {
		t.Fatal(err)
	}
	data, err := w.Seal()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenGraphSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gs, gd := walkAll(t, s)
	if gs[0] != MaxNodeID || !slices.Equal(gd[0], []uint64{0, MaxNodeID}) {
		t.Fatalf("extremes: %v %v", gs, gd)
	}
}

func TestGraphSegmentRejectsDisorder(t *testing.T) {
	w := &GraphSegmentWriter{}
	if err := w.Add(10, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(10, nil); err == nil {
		t.Fatal("duplicate source accepted")
	}
	if err := w.Add(9, nil); err == nil {
		t.Fatal("regressing source accepted")
	}
	if err := w.Add(11, []uint64{5, 5}); err == nil {
		t.Fatal("duplicate destination accepted")
	}
	if _, err := (&GraphSegmentWriter{}).Seal(); err == nil {
		t.Fatal("empty graph segment sealed")
	}
}

func TestGraphSegmentFlipSweep(t *testing.T) {
	srcs, dsts := graphCorpus(300)
	good, err := sealGraph(srcs, dsts, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s0, err := OpenGraphSegment(good)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	wantSrcs, wantDsts := walkAll(t, s0)

	// No dictionary in this format: only the writer field may accept a
	// flip, and there the walk must be identical.
	for i := 0; i < len(good); i += 5 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		s, err := OpenGraphSegment(bad)
		if err != nil {
			continue
		}
		var gs []uint64
		var gd [][]uint64
		werr := s.Walk(func(src uint64, d []uint64) error {
			gs = append(gs, src)
			gd = append(gd, slices.Clone(d))
			return nil
		})
		s.Close()
		if werr != nil {
			continue
		}
		if i < 24 || i >= 32 {
			t.Fatalf("flip at %d accepted silently", i)
		}
		if !reflect.DeepEqual(gs, wantSrcs) || !reflect.DeepEqual(gd, wantDsts) {
			t.Fatalf("flip at %d changed the walk without an error", i)
		}
	}
}

func FuzzOpenGraphSegment(f *testing.F) {
	srcs, dsts := graphCorpus(50)
	for _, target := range []int{0, 512} {
		data, err := sealGraph(srcs, dsts, target)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := OpenGraphSegment(data)
		if err != nil {
			return
		}
		defer s.Close()
		prev := uint64(0)
		first := true
		if err := s.Walk(func(src uint64, d []uint64) error {
			if !first && src <= prev {
				t.Fatal("walk yielded non-increasing sources")
			}
			first, prev = false, src
			for j := 1; j < len(d); j++ {
				if d[j] <= d[j-1] {
					t.Fatal("walk yielded non-increasing destinations")
				}
			}
			return nil
		}); err != nil {
			return
		}
	})
}
