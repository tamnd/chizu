package crawl

import "bytes"

// simhash is the 64-bit Charikar sketch over whitespace tokens with
// FNV-1a features, the sketch every stored row carries (CR-I3) and the
// doc 06 near-dup pass reads back.
//
// The vote loop is SWAR-packed: each accumulator word holds four 16-bit
// lanes and a nibble table adds 0 or 2 per lane, so one token costs 16
// adds instead of 64. A lane holds 2*ones for its bit and the vote
// 2*ones-n applies at flush, before any lane can reach 16 bits. The
// importer lab measured the branchy form 4x slower than branchless and
// this form cuts the branchless one down again on 7.7 KiB pages.
func simhash(text []byte) uint64 {
	const offset64, prime64 = 14695981039346656037, 1099511628211
	var acc [64]int32
	var lanes [16]uint64
	chunk := int32(0)
	flush := func() {
		for w := range lanes {
			for l := range 4 {
				acc[4*w+l] += int32(lanes[w]>>(16*l)&0xFFFF) - chunk
			}
			lanes[w] = 0
		}
		chunk = 0
	}
	for tok := range bytes.FieldsSeq(text) {
		f := uint64(offset64)
		for _, c := range tok {
			f = (f ^ uint64(c)) * prime64
		}
		for w := range lanes {
			lanes[w] += nibSpread[f>>(4*w)&0xF]
		}
		if chunk++; chunk == 32000 {
			flush()
		}
	}
	flush()
	var out uint64
	for b := range 64 {
		if acc[b] > 0 {
			out |= 1 << b
		}
	}
	return out
}

// nibSpread[n] spreads nibble n across four 16-bit lanes, 2 per set bit.
var nibSpread = func() (t [16]uint64) {
	for n := range t {
		for l := range 4 {
			t[n] |= uint64(n>>l&1) * 2 << (16 * l)
		}
	}
	return
}()
