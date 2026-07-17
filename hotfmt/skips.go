package hotfmt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Skips band, doc 05 section 6: per term, a fixed-shape region at
// skip_off holding L1 entries (one per postings block), a parallel
// array of positions offsets, and L2 superskip entries (one per 32 L1
// entries). The whole region is derivable from df, so it carries no
// counts of its own.

const (
	skipL1Size    = 10
	skipPosSize   = 5
	skipL2Size    = 12
	skipL2Fanout  = 32
	skipBlockDocs = PostingsBlockLen
)

// SkipL1 is one L1 entry: the block's last docid, its impact bound,
// and its byte offset from the term's postings_off.
type SkipL1 struct {
	LastDocid uint32
	Impact    uint8
	Off       uint64
}

// SkipL2 is one derived superskip entry over a group of 32 L1 entries.
type SkipL2 struct {
	LastDocid uint32
	Impact    uint8
	L1Index   uint32
}

// SkipCounts returns the L1 and L2 entry counts for a term of the
// given df.
func SkipCounts(df uint32) (nb, nl2 int) {
	nb = int((uint64(df) + skipBlockDocs - 1) / skipBlockDocs)
	nl2 = (nb + skipL2Fanout - 1) / skipL2Fanout
	return nb, nl2
}

// SkipRegionSize returns the byte size of a term's skip region.
func SkipRegionSize(df uint32) int {
	nb, nl2 := SkipCounts(df)
	return nb*(skipL1Size+skipPosSize) + nl2*skipL2Size
}

// EncodeSkips lays out one term's skip region. l1 comes straight from
// EncodePostings and posOffs is the parallel positions offset per
// block; L2 is derived here, never supplied.
func EncodeSkips(dst []byte, l1 []SkipL1, posOffs []uint64) ([]byte, error) {
	if len(l1) == 0 || len(posOffs) != len(l1) {
		return nil, errors.New("hotfmt: skip arrays empty or mismatched")
	}
	for i, e := range l1 {
		if e.Off > maxU40 {
			return nil, errors.New("hotfmt: skip offset exceeds u40")
		}
		if i > 0 {
			if e.LastDocid <= l1[i-1].LastDocid || e.Off <= l1[i-1].Off {
				return nil, errors.New("hotfmt: skip entries not strictly increasing")
			}
		}
		dst = binary.LittleEndian.AppendUint32(dst, e.LastDocid)
		dst = append(dst, e.Impact)
		dst = appendU40(dst, e.Off)
	}
	for _, p := range posOffs {
		if p > maxU40 {
			return nil, errors.New("hotfmt: positions offset exceeds u40")
		}
		dst = appendU40(dst, p)
	}
	for g := 0; g < len(l1); g += skipL2Fanout {
		end := min(g+skipL2Fanout, len(l1))
		var impact uint8
		for _, e := range l1[g:end] {
			impact = max(impact, e.Impact)
		}
		dst = binary.LittleEndian.AppendUint32(dst, l1[end-1].LastDocid)
		dst = append(dst, impact)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(g))
		dst = append(dst, 0, 0, 0)
	}
	return dst, nil
}

// ParseSkips decodes a term's whole skip region for the given df and
// checks that the L2 level really is the derivation of L1.
func ParseSkips(data []byte, df uint32) ([]SkipL1, []uint64, []SkipL2, error) {
	nb, nl2 := SkipCounts(df)
	if nb == 0 {
		return nil, nil, nil, errors.New("hotfmt: skip region for df 0")
	}
	if len(data) != SkipRegionSize(df) {
		return nil, nil, nil, fmt.Errorf("hotfmt: skip region is %d bytes, want %d", len(data), SkipRegionSize(df))
	}
	l1 := make([]SkipL1, nb)
	for i := range l1 {
		e := data[i*skipL1Size:]
		l1[i] = SkipL1{
			LastDocid: binary.LittleEndian.Uint32(e),
			Impact:    e[4],
			Off:       u40(e[5:]),
		}
		if i > 0 && (l1[i].LastDocid <= l1[i-1].LastDocid || l1[i].Off <= l1[i-1].Off) {
			return nil, nil, nil, errors.New("hotfmt: skip entries not strictly increasing")
		}
	}
	pos := make([]uint64, nb)
	pbase := nb * skipL1Size
	for i := range pos {
		pos[i] = u40(data[pbase+i*skipPosSize:])
	}
	l2 := make([]SkipL2, nl2)
	lbase := pbase + nb*skipPosSize
	for g := range l2 {
		e := data[lbase+g*skipL2Size:]
		l2[g] = SkipL2{
			LastDocid: binary.LittleEndian.Uint32(e),
			Impact:    e[4],
			L1Index:   binary.LittleEndian.Uint32(e[5:]),
		}
		if e[9] != 0 || e[10] != 0 || e[11] != 0 {
			return nil, nil, nil, errors.New("hotfmt: nonzero skip padding")
		}
		start := g * skipL2Fanout
		end := min(start+skipL2Fanout, nb)
		var impact uint8
		for _, le := range l1[start:end] {
			impact = max(impact, le.Impact)
		}
		if l2[g].L1Index != uint32(start) || l2[g].LastDocid != l1[end-1].LastDocid || l2[g].Impact != impact {
			return nil, nil, nil, errors.New("hotfmt: superskip entry disagrees with L1")
		}
	}
	return l1, pos, l2, nil
}
