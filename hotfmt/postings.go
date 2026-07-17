package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Postings band, doc 05 section 5: per term, 128-entry FOR blocks with
// up to 4 patched exceptions, then one vbyte tail block for the
// remainder. Every block carries an 8-byte header whose impact byte is
// the block's quantized maximum phase-1 contribution, the bound the
// skips band republishes and block-max traversal prunes on.

const (
	// PostingsBlockLen is the FOR block size; the impact-resolution
	// argument caps it and the header-overhead argument floors it.
	PostingsBlockLen = 128

	postingsHeaderSize = 8
	maxPatches         = 4

	postingsMixed byte = 1 << 0 // per-doc fieldmask bytes present
	postingsVbyte byte = 1 << 1 // tail block, vbyte encoded
	patchShift         = 2      // flag bits 2-4 carry the patch count
)

// docidWidths maps the 4-bit width nibble to bit widths; gaps past 8
// trade a little packing slack for a table that still reaches u32.
var docidWidths = [16]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 16, 20, 24, 28, 32}

// Posting is one term-document hit as the builder hands it over.
// Impact is the doc 08 quantized phase-1 bound for this posting; the
// block stores the maximum.
type Posting struct {
	Docid  uint32
	TF     uint8
	Mask   uint8
	Impact uint8
}

// PostingsBlock is one decoded block's header view.
type PostingsBlock struct {
	LastDocid uint32
	Impact    uint8
	NEntries  int
	Tail      bool
}

// EncodePostings encodes one term's whole postings list and returns
// the bytes plus per-block skip seeds (offset relative to the term's
// postings_off). Docids are local, strictly increasing; tf >= 1; mask
// in [1, maxFieldMask].
func EncodePostings(ps []Posting) ([]byte, []SkipL1, error) {
	if len(ps) == 0 {
		return nil, nil, errors.New("hotfmt: empty postings list")
	}
	for i, p := range ps {
		if i > 0 && p.Docid <= ps[i-1].Docid {
			return nil, nil, errors.New("hotfmt: postings not strictly increasing")
		}
		if p.TF == 0 || p.Mask == 0 || p.Mask > maxFieldMask {
			return nil, nil, fmt.Errorf("hotfmt: bad posting for docid %d", p.Docid)
		}
	}
	var out []byte
	var l1 []SkipL1
	prev := int64(-1)
	for start := 0; start < len(ps); start += PostingsBlockLen {
		end := min(start+PostingsBlockLen, len(ps))
		block := ps[start:end]
		off := uint64(len(out))
		var err error
		if len(block) == PostingsBlockLen {
			out, err = appendFORBlock(out, block, prev)
		} else {
			out, err = appendVbyteBlock(out, block, prev)
		}
		if err != nil {
			return nil, nil, err
		}
		l1 = append(l1, SkipL1{
			LastDocid: block[len(block)-1].Docid,
			Impact:    blockImpact(block),
			Off:       off,
		})
		prev = int64(block[len(block)-1].Docid)
	}
	return out, l1, nil
}

func blockImpact(block []Posting) uint8 {
	var m uint8
	for _, p := range block {
		m = max(m, p.Impact)
	}
	return m
}

// blockGaps fills gaps[i] = docid[i] - previous - 1, so every stored
// value is >= 0 and the chain needs no first-entry special case: the
// term starts from a virtual previous of -1.
func blockGaps(gaps []uint32, block []Posting, prev int64) {
	for i, p := range block {
		gaps[i] = uint32(int64(p.Docid) - prev - 1)
		prev = int64(p.Docid)
	}
}

func mixedMasks(block []Posting) bool {
	for _, p := range block {
		if p.Mask != 1 {
			return true
		}
	}
	return false
}

func appendHeader8(dst []byte, last uint32, impact, widths, flags byte, n int) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, last)
	return append(dst, impact, widths, flags, byte(n))
}

func appendFORBlock(dst []byte, block []Posting, prev int64) ([]byte, error) {
	var gaps [PostingsBlockLen]uint32
	blockGaps(gaps[:], block, prev)

	widx, patches := chooseWidth(gaps[:])
	w := docidWidths[widx]

	var tfmax uint8
	for _, p := range block {
		tfmax = max(tfmax, p.TF-1)
	}
	tfw := uint8(bits.Len8(tfmax))

	flags := byte(len(patches)) << patchShift
	mixed := mixedMasks(block)
	if mixed {
		flags |= postingsMixed
	}
	dst = appendHeader8(dst, block[len(block)-1].Docid, blockImpact(block), byte(widx)|tfw<<4, flags, len(block))
	for _, pi := range patches {
		dst = append(dst, byte(pi))
		dst = binary.LittleEndian.AppendUint32(dst, gaps[pi])
	}
	packed := gaps
	for _, pi := range patches {
		packed[pi] = 0
	}
	dst = appendPacked(dst, packed[:], w)
	var tfs [PostingsBlockLen]uint32
	for i, p := range block {
		tfs[i] = uint32(p.TF - 1)
	}
	dst = appendPacked(dst, tfs[:len(block)], tfw)
	if mixed {
		for _, p := range block {
			dst = append(dst, p.Mask)
		}
	}
	return dst, nil
}

// chooseWidth picks the packing width minimizing packed-plus-patch
// bytes with at most maxPatches exceptions; ties go to the narrower
// width. Deterministic, so the encoding is canonical.
func chooseWidth(gaps []uint32) (widx int, patches []int) {
	bestSize := -1
	var best []int
	for cand := range docidWidths {
		w := docidWidths[cand]
		var exc []int
		for i, g := range gaps {
			if w < 32 && g >= 1<<w {
				exc = append(exc, i)
			}
			if len(exc) > maxPatches {
				break
			}
		}
		if len(exc) > maxPatches {
			continue
		}
		size := packedLen(len(gaps), w) + 5*len(exc)
		if bestSize < 0 || size < bestSize {
			bestSize, widx, best = size, cand, exc
		}
	}
	return widx, best
}

func packedLen(n int, w uint8) int {
	return (n*int(w) + 7) / 8
}

// appendPacked bitpacks vals LSB-first at width w; padding bits are
// zero so the encoding is canonical.
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

func appendVbyteBlock(dst []byte, block []Posting, prev int64) ([]byte, error) {
	var gaps [PostingsBlockLen]uint32
	blockGaps(gaps[:len(block)], block, prev)
	flags := postingsVbyte
	mixed := mixedMasks(block)
	if mixed {
		flags |= postingsMixed
	}
	dst = appendHeader8(dst, block[len(block)-1].Docid, blockImpact(block), 0, flags, len(block))
	for _, g := range gaps[:len(block)] {
		dst = appendUvarint(dst, uint64(g))
	}
	for _, p := range block {
		dst = append(dst, p.TF)
	}
	if mixed {
		for _, p := range block {
			dst = append(dst, p.Mask)
		}
	}
	return dst, nil
}

// DecodePostingsBlock decodes the block at the front of data. prev is
// the docid chain base: -1 at the start of a term, otherwise the
// previous block's last docid. The three slices must be at least
// PostingsBlockLen long; masks fills with 1 when the block is not
// mixed. Returns the header view and the bytes consumed.
func DecodePostingsBlock(data []byte, prev int64, docids []uint32, tfs, masks []uint8) (PostingsBlock, int, error) {
	var b PostingsBlock
	if len(data) < postingsHeaderSize {
		return b, 0, errors.New("hotfmt: postings block truncated")
	}
	b.LastDocid = binary.LittleEndian.Uint32(data)
	b.Impact = data[4]
	widths := data[5]
	flags := data[6]
	b.NEntries = int(data[7])
	b.Tail = flags&postingsVbyte != 0
	if b.NEntries == 0 || b.NEntries > PostingsBlockLen {
		return b, 0, fmt.Errorf("hotfmt: postings block with %d entries", b.NEntries)
	}
	if !b.Tail && b.NEntries != PostingsBlockLen {
		return b, 0, errors.New("hotfmt: short FOR block")
	}
	if flags&^(postingsMixed|postingsVbyte|0b11100) != 0 {
		return b, 0, errors.New("hotfmt: unknown postings flag")
	}
	n := b.NEntries
	pos := postingsHeaderSize
	if b.Tail {
		if widths != 0 || flags>>patchShift&0b111 != 0 {
			return b, 0, errors.New("hotfmt: vbyte block with widths or patches")
		}
		for i := range n {
			g, gn, err := uvarint(data[pos:])
			if err != nil {
				return b, 0, err
			}
			if g > 1<<32-1 {
				return b, 0, errors.New("hotfmt: docid gap exceeds u32")
			}
			pos += gn
			docids[i] = uint32(g)
		}
		if len(data) < pos+n {
			return b, 0, errors.New("hotfmt: postings block truncated")
		}
		for i := range n {
			tfs[i] = data[pos+i]
		}
		pos += n
	} else {
		widx := widths & 0x0F
		tfw := widths >> 4
		if tfw > 8 {
			return b, 0, errors.New("hotfmt: tf width past 8 bits")
		}
		npatch := int(flags >> patchShift & 0b111)
		if npatch > maxPatches {
			return b, 0, fmt.Errorf("hotfmt: %d patches", npatch)
		}
		type patch struct {
			idx int
			val uint32
		}
		var patches [maxPatches]patch
		if len(data) < pos+5*npatch {
			return b, 0, errors.New("hotfmt: postings block truncated")
		}
		for i := range npatch {
			patches[i] = patch{int(data[pos]), binary.LittleEndian.Uint32(data[pos+1:])}
			pos += 5
			if patches[i].idx >= n || (i > 0 && patches[i].idx <= patches[i-1].idx) {
				return b, 0, errors.New("hotfmt: patch positions not strictly increasing")
			}
		}
		w := docidWidths[widx]
		need := packedLen(n, w) + packedLen(n, tfw)
		if len(data) < pos+need {
			return b, 0, errors.New("hotfmt: postings block truncated")
		}
		unpack(data[pos:], docids[:n], w)
		pos += packedLen(n, w)
		var tfvals [PostingsBlockLen]uint32
		unpack(data[pos:], tfvals[:n], tfw)
		pos += packedLen(n, tfw)
		for i := range n {
			if tfvals[i] > 254 {
				return b, 0, errors.New("hotfmt: tf exceeds 255")
			}
			tfs[i] = uint8(tfvals[i] + 1)
		}
		for i := range npatch {
			if docids[patches[i].idx] != 0 {
				return b, 0, errors.New("hotfmt: patched slot not zero in packed stream")
			}
			docids[patches[i].idx] = patches[i].val
		}
	}
	// The docids slice holds gaps at this point; resolve the chain.
	for i := range n {
		next := prev + int64(docids[i]) + 1
		if next > 1<<32-1 {
			return b, 0, errors.New("hotfmt: docid overflows u32")
		}
		docids[i] = uint32(next)
		prev = next
	}
	if docids[n-1] != b.LastDocid {
		return b, 0, errors.New("hotfmt: last docid disagrees with header")
	}
	if flags&postingsMixed != 0 {
		if len(data) < pos+n {
			return b, 0, errors.New("hotfmt: postings block truncated")
		}
		for i := range n {
			m := data[pos+i]
			if m == 0 || m > maxFieldMask {
				return b, 0, fmt.Errorf("hotfmt: fieldmask 0x%02X", m)
			}
			masks[i] = m
		}
		pos += n
	} else {
		for i := range n {
			masks[i] = 1
		}
	}
	if b.Tail {
		for i := range n {
			if tfs[i] == 0 {
				return b, 0, errors.New("hotfmt: tf 0")
			}
		}
	}
	return b, pos, nil
}
