package main

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/tamnd/chizu/hotfmt"
)

// The lab measures copies of production kernels at parameterized
// lengths. These tests pin the copies to hotfmt byte for byte at block
// 128 so the sweep cannot drift from what it claims to measure.

func TestLabBlockMatchesHotfmt(t *testing.T) {
	rng := rand.New(rand.NewSource(2107))
	for _, tier := range tiers {
		lists := hotLists(rng, tier.w, tier.tailPct, 4096)
		if len(lists) == 0 {
			t.Fatalf("%s: no lists", tier.name)
		}
		for _, l := range lists {
			block := hotfmt.PostingsBlockLen
			var docids, want [hotfmt.PostingsBlockLen]uint32
			var tfs, masks, wantTFs, wantMasks [hotfmt.PostingsBlockLen]uint8
			prev := int64(-1)
			for bi := range l.l1 {
				data := l.enc[l.l1[bi].Off:]
				b, hn, err := hotfmt.DecodePostingsBlock(data, prev, want[:], wantTFs[:], wantMasks[:])
				if err != nil {
					t.Fatalf("%s block %d: hotfmt decode: %v", tier.name, bi, err)
				}
				ln, err := decodeLabBlock(data, block, prev, docids[:], tfs[:], masks[:])
				if err != nil {
					t.Fatalf("%s block %d: lab decode: %v", tier.name, bi, err)
				}
				if ln != hn {
					t.Fatalf("%s block %d: consumed %d, hotfmt %d", tier.name, bi, ln, hn)
				}
				if docids != want || tfs != wantTFs || masks != wantMasks {
					t.Fatalf("%s block %d: lab decode disagrees with hotfmt", tier.name, bi)
				}

				// The lab encoder must reproduce hotfmt's bytes exactly.
				gaps := make([]uint32, block)
				bt := make([]uint8, block)
				p := prev
				for i := range block {
					ps := l.ps[bi*block+i]
					gaps[i] = uint32(int64(ps.Docid) - p - 1)
					bt[i] = ps.TF
					p = int64(ps.Docid)
				}
				got, err := encodeLabBlock(nil, gaps, bt, prev)
				if err != nil {
					t.Fatalf("%s block %d: lab encode: %v", tier.name, bi, err)
				}
				if !bytes.Equal(got, data[:hn]) {
					t.Fatalf("%s block %d: lab encode differs from hotfmt bytes", tier.name, bi)
				}
				prev = int64(b.LastDocid)
			}
		}
	}
}

func TestPackRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, n := range []int{5, 64, 128, 256} {
		for _, w := range widthMenu {
			limit := uint32(uint64(1)<<w - 1)
			src := make([]uint32, n)
			for i := range src {
				src[i] = uint32(rng.Intn(int(limit) + 1))
			}
			packed := appendPacked(nil, src, w)
			if len(packed) != packedLen(n, w) {
				t.Fatalf("n=%d w=%d: packed %d bytes, want %d", n, w, len(packed), packedLen(n, w))
			}
			got := make([]uint32, n)
			unpack(packed, got, w)
			for i := range src {
				if got[i] != src[i] {
					t.Fatalf("n=%d w=%d: vals[%d] = %d, want %d", n, w, i, got[i], src[i])
				}
			}
		}
	}
}

func TestVbyteRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	src := make([]uint32, 128)
	for i := range src {
		src[i] = rng.Uint32() >> uint(rng.Intn(32))
	}
	packed := appendVbyte(nil, src)
	got := make([]uint32, len(src))
	if n := unpackVbyte(packed, got); n != len(packed) {
		t.Fatalf("consumed %d of %d bytes", n, len(packed))
	}
	for i := range src {
		if got[i] != src[i] {
			t.Fatalf("vals[%d] = %d, want %d", i, got[i], src[i])
		}
	}
}

func TestLabBlockRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, n := range []int{64, 128, 256} {
		for _, tier := range tiers {
			prev := int64(-1)
			for round := range 8 {
				gaps := gapStream(rng, n, tier.w, tier.tailPct)
				tfs := tfStream(rng, n)
				if round == 0 {
					for i := range tfs {
						tfs[i] = 1 // tfw 0 path
					}
				}
				enc, err := encodeLabBlock(nil, gaps, tfs, prev)
				if err != nil {
					t.Fatalf("n=%d %s: encode: %v", n, tier.name, err)
				}
				docids := make([]uint32, n)
				gotTFs := make([]uint8, n)
				masks := make([]uint8, n)
				consumed, err := decodeLabBlock(enc, n, prev, docids, gotTFs, masks)
				if err != nil {
					t.Fatalf("n=%d %s: decode: %v", n, tier.name, err)
				}
				if consumed != len(enc) {
					t.Fatalf("n=%d %s: consumed %d of %d", n, tier.name, consumed, len(enc))
				}
				d := prev
				for i := range n {
					d += int64(gaps[i]) + 1
					if docids[i] != uint32(d) {
						t.Fatalf("n=%d %s: docids[%d] = %d, want %d", n, tier.name, i, docids[i], d)
					}
					if gotTFs[i] != tfs[i] {
						t.Fatalf("n=%d %s: tfs[%d] = %d, want %d", n, tier.name, i, gotTFs[i], tfs[i])
					}
					if masks[i] != 1 {
						t.Fatalf("n=%d %s: masks[%d] = %d", n, tier.name, i, masks[i])
					}
				}
				prev = -1
				if round%2 == 0 {
					prev = d % (docidCap / 2)
				}
			}
		}
	}
}

func TestLabBlockWidthZero(t *testing.T) {
	// Consecutive docids pack at width 0: no gap bytes at all.
	gaps := make([]uint32, 64)
	tfs := make([]uint8, 64)
	for i := range tfs {
		tfs[i] = 1
	}
	enc, err := encodeLabBlock(nil, gaps, tfs, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != labHeaderSize {
		t.Fatalf("encoded %d bytes, want header only %d", len(enc), labHeaderSize)
	}
	docids := make([]uint32, 64)
	gotTFs := make([]uint8, 64)
	masks := make([]uint8, 64)
	if _, err := decodeLabBlock(enc, 64, -1, docids, gotTFs, masks); err != nil {
		t.Fatal(err)
	}
	for i := range docids {
		if docids[i] != uint32(i) {
			t.Fatalf("docids[%d] = %d", i, docids[i])
		}
	}
}

func TestTombSet(t *testing.T) {
	set := make(tombSet, 4)
	for _, d := range []uint32{0, 63, 64, 200} {
		set[d>>6] |= 1 << (d & 63)
	}
	for _, d := range []uint32{0, 63, 64, 200} {
		if !set.test(d) {
			t.Fatalf("docid %d should be set", d)
		}
	}
	for _, d := range []uint32{1, 62, 65, 199, 255} {
		if set.test(d) {
			t.Fatalf("docid %d should be clear", d)
		}
	}
}
