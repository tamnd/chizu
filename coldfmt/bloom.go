package coldfmt

import "encoding/binary"

// The urlfp bloom, doc 04 section 3.2: blocked, 10 bits per row, ~1% false
// positives, for chizu admin point lookups and delta-build candidate
// checks. No bulk path uses it, so the rate is a convenience knob.
//
// Each block is 512 bits (one cache line). The urlfp is already a
// fingerprint, so its two halves serve directly as hashes: the first eight
// bytes pick the block, seven 9-bit slices of the last eight pick the bits.

const bloomBitsPerRow = 10

func bloomBuild(rows []PageRow) []byte {
	nblocks := (len(rows)*bloomBitsPerRow + 511) / 512
	buf := make([]byte, nblocks*64)
	for i := range rows {
		blk := binary.LittleEndian.Uint64(rows[i].URLFP[:8]) % uint64(nblocks)
		h := binary.LittleEndian.Uint64(rows[i].URLFP[8:])
		for k := range 7 {
			bit := (h >> (9 * k)) & 511
			buf[blk*64+bit/8] |= 1 << (bit % 8)
		}
	}
	return buf
}

func bloomTest(bloom []byte, fp [16]byte) bool {
	nblocks := uint64(len(bloom) / 64)
	if nblocks == 0 {
		return false
	}
	blk := binary.LittleEndian.Uint64(fp[:8]) % nblocks
	h := binary.LittleEndian.Uint64(fp[8:])
	for k := range 7 {
		bit := (h >> (9 * k)) & 511
		if bloom[blk*64+bit/8]&(1<<(bit%8)) == 0 {
			return false
		}
	}
	return true
}
