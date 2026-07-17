package main

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"math/rand"
)

// The pack and unpack kernels are byte-for-byte copies of hotfmt's
// (postings.go), which are already length-parameterized; the lab sweeps
// them at block 64/128/256 where production fixes 128. The test file
// cross-checks the copies against hotfmt on real encoded blocks so the
// lab cannot drift from what it claims to measure.

// widthMenu mirrors hotfmt's docidWidths without the 0 entry.
var widthMenu = []uint8{1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 16, 20, 24, 28, 32}

func packedLen(n int, w uint8) int {
	return (n*int(w) + 7) / 8
}

func appendPacked(dst []byte, vals []uint32, w uint8) []byte {
	if w == 0 {
		return dst
	}
	var acc uint64
	var nbits uint
	for _, v := range vals {
		acc |= uint64(v) << nbits
		nbits += uint(w)
		for nbits >= 8 {
			dst = append(dst, byte(acc))
			acc >>= 8
			nbits -= 8
		}
	}
	if nbits > 0 {
		dst = append(dst, byte(acc))
	}
	return dst
}

func unpack(data []byte, vals []uint32, w uint8) {
	if w == 0 {
		for i := range vals {
			vals[i] = 0
		}
		return
	}
	var acc uint64
	var nbits uint
	pos := 0
	mask := uint64(1)<<w - 1
	for i := range vals {
		for nbits < uint(w) {
			acc |= uint64(data[pos]) << nbits
			pos++
			nbits += 8
		}
		vals[i] = uint32(acc & mask)
		acc >>= w
		nbits -= uint(w)
	}
}

// appendVbyte is the baseline codec: plain LEB128 varints, the format
// the FOR blocks are being compared against.
func appendVbyte(dst []byte, vals []uint32) []byte {
	for _, v := range vals {
		dst = binary.AppendUvarint(dst, uint64(v))
	}
	return dst
}

// unpackVbyte decodes n varints with the loop inlined the way a real
// decoder would write it, not through binary.Uvarint's error paths.
func unpackVbyte(data []byte, vals []uint32) int {
	pos := 0
	for i := range vals {
		var v uint32
		var shift uint
		for {
			b := data[pos]
			pos++
			v |= uint32(b&0x7F) << shift
			if b < 0x80 {
				break
			}
			shift += 7
		}
		vals[i] = v
	}
	return pos
}

// labBlock is the production block shape at a parameterized length:
// 8-byte header (last docid u32, impact, widths, flags, nentries),
// bitpacked gaps with up to 4 patched exceptions, bitpacked tf-1.
// Masks stay off (the common non-mixed case) so the sweep measures the
// docid+tf spine that every query pays.
const (
	labHeaderSize = 8
	labMaxPatches = 4
	labImpact     = 50 // constant across the lab; impact resolution is C3's business
)

var docidWidths = [16]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 16, 20, 24, 28, 32}

// labMaxBlock bounds the sweep's block sizes so tf staging can stay a
// stack array the way production's PostingsBlockLen array does.
const labMaxBlock = 256

// encodeLabBlock packs one block of gaps and tfs with hotfmt's exact
// FOR layout at a parameterized length: header, patches, packed gaps,
// packed tf-1. Width choice mirrors chooseWidth (candidates from width
// 0 up, exceptions at g >= 1<<w, ties to the narrower width) and tfw
// mirrors bits.Len8 of the max tf-1. prev is the previous block's last
// docid, -1 for the first block.
func encodeLabBlock(dst []byte, gaps []uint32, tfs []uint8, prev int64) ([]byte, error) {
	n := len(gaps)
	bestSize := -1
	bestIdx := 0
	var bestPatches []int
	for cand := range docidWidths {
		w := docidWidths[cand]
		var exc []int
		for i, g := range gaps {
			if w < 32 && g >= 1<<w {
				exc = append(exc, i)
			}
			if len(exc) > labMaxPatches {
				break
			}
		}
		if len(exc) > labMaxPatches {
			continue
		}
		size := packedLen(n, w) + 5*len(exc)
		if bestSize < 0 || size < bestSize {
			bestSize, bestIdx, bestPatches = size, cand, exc
		}
	}
	w := docidWidths[bestIdx]
	var tfmax uint8
	for _, tf := range tfs {
		tfmax = max(tfmax, tf-1)
	}
	tfw := uint8(bits.Len8(tfmax))

	last := prev
	for _, g := range gaps {
		last += int64(g) + 1
	}
	if last > 0xFFFFFFFF {
		return nil, fmt.Errorf("docid space exceeded")
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(last))
	dst = append(dst, labImpact)
	dst = append(dst, uint8(bestIdx)|tfw<<4)     // widths
	dst = append(dst, byte(len(bestPatches))<<2) // flags: patches only
	dst = append(dst, byte(n))                   // nentries (256 wraps; decode takes n)
	for _, pi := range bestPatches {
		dst = append(dst, byte(pi))
		dst = binary.LittleEndian.AppendUint32(dst, gaps[pi])
	}
	packed := make([]uint32, n)
	copy(packed, gaps)
	for _, pi := range bestPatches {
		packed[pi] = 0
	}
	dst = appendPacked(dst, packed, w)
	tvals := make([]uint32, n)
	for i, tf := range tfs {
		tvals[i] = uint32(tf - 1)
	}
	dst = appendPacked(dst, tvals, tfw)
	return dst, nil
}

// decodeLabBlock is the measured kernel: hotfmt.DecodePostingsBlock's
// non-mixed FOR path at a parameterized length, every validation
// branch production keeps included, masks filled with 1 the way the
// non-mixed path does.
func decodeLabBlock(data []byte, n int, prev int64, docids []uint32, tfs, masks []uint8) (int, error) {
	if len(data) < labHeaderSize {
		return 0, fmt.Errorf("short block")
	}
	last := binary.LittleEndian.Uint32(data)
	widths := data[5]
	flags := data[6]
	widx := widths & 0x0F
	tfw := widths >> 4
	if tfw > 8 {
		return 0, fmt.Errorf("tf width past 8 bits")
	}
	npatch := int(flags >> 2 & 0b111)
	if npatch > labMaxPatches {
		return 0, fmt.Errorf("%d patches", npatch)
	}
	type patch struct {
		idx int
		val uint32
	}
	var patches [labMaxPatches]patch
	pos := labHeaderSize
	if len(data) < pos+5*npatch {
		return 0, fmt.Errorf("short block")
	}
	for i := range npatch {
		patches[i] = patch{int(data[pos]), binary.LittleEndian.Uint32(data[pos+1:])}
		pos += 5
		if patches[i].idx >= n || (i > 0 && patches[i].idx <= patches[i-1].idx) {
			return 0, fmt.Errorf("patch positions not strictly increasing")
		}
	}
	w := docidWidths[widx]
	need := packedLen(n, w) + packedLen(n, tfw)
	if len(data) < pos+need {
		return 0, fmt.Errorf("short block")
	}
	unpack(data[pos:], docids[:n], w)
	pos += packedLen(n, w)
	var tfvals [labMaxBlock]uint32
	unpack(data[pos:], tfvals[:n], tfw)
	pos += packedLen(n, tfw)
	for i := range n {
		if tfvals[i] > 254 {
			return 0, fmt.Errorf("tf exceeds 255")
		}
		tfs[i] = uint8(tfvals[i] + 1)
	}
	for i := range npatch {
		if docids[patches[i].idx] != 0 {
			return 0, fmt.Errorf("patched slot not zero in packed stream")
		}
		docids[patches[i].idx] = patches[i].val
	}
	d := prev
	for i := range n {
		d += int64(docids[i]) + 1
		if d > 0xFFFFFFFF {
			return 0, fmt.Errorf("docid overflows u32")
		}
		docids[i] = uint32(d)
	}
	if docids[n-1] != last {
		return 0, fmt.Errorf("last docid disagrees")
	}
	for i := range n {
		masks[i] = 1
	}
	return pos, nil
}

// tombSet is the resident exclusion bitset from hotfmt's BuildExclusion,
// docid-indexed, no hashing.
type tombSet []uint64

func (s tombSet) test(docid uint32) bool {
	return s[docid>>6]>>(docid&63)&1 != 0
}

// gapStream draws gaps whose magnitudes cluster around a width tier,
// with a small heavy tail so patch slots get exercised at production
// rates (a few percent).
func gapStream(rng *rand.Rand, n int, w uint8, tailPct int) []uint32 {
	gaps := make([]uint32, n)
	limit := uint32(uint64(1)<<w - 1)
	for i := range gaps {
		if rng.Intn(100) < tailPct {
			gaps[i] = limit + uint32(rng.Intn(1<<16))
		} else {
			gaps[i] = uint32(rng.Intn(int(limit) + 1))
		}
	}
	return gaps
}

func tfStream(rng *rand.Rand, n int) []uint8 {
	tfs := make([]uint8, n)
	for i := range tfs {
		// Zipf-ish: mostly 1, a tail up to 8.
		tf := 1
		for tf < 8 && rng.Intn(3) == 0 {
			tf++
		}
		tfs[i] = uint8(tf)
	}
	return tfs
}
