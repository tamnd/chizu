package crawl

import "bytes"

// simhash is the 64-bit Charikar sketch over whitespace tokens with
// FNV-1a features, the sketch every stored row carries (CR-I3) and the
// doc 06 near-dup pass reads back. The vote loop is branchless: the
// importer lab measured the branchy form 4x slower and it dominated
// the hash stage.
func simhash(text []byte) uint64 {
	const offset64, prime64 = 14695981039346656037, 1099511628211
	var acc [64]int32
	for tok := range bytes.FieldsSeq(text) {
		f := uint64(offset64)
		for _, c := range tok {
			f = (f ^ uint64(c)) * prime64
		}
		for b := range 64 {
			acc[b] += int32(f>>b&1)*2 - 1
		}
	}
	var out uint64
	for b := range 64 {
		if acc[b] > 0 {
			out |= 1 << b
		}
	}
	return out
}
