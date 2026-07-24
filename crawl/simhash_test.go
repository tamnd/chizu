package crawl

import (
	"bytes"
	"fmt"
	"testing"
)

// simhashRef is the plain one-counter-per-bit form the SWAR loop replaced;
// the packed loop must agree with it exactly.
func simhashRef(text []byte) uint64 {
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

func TestSimhashMatchesReference(t *testing.T) {
	long := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog "), 4000) // ~36k tokens, crosses the flush chunk
	cases := [][]byte{
		nil,
		[]byte("   \t\n  "),
		[]byte("one"),
		[]byte("the quick brown fox jumps over the lazy dog"),
		[]byte("completely different words about nothing similar here"),
		bytes.Repeat([]byte("aa bb cc dd ee ff gg hh "), 500),
		long,
	}
	for i := range 64 {
		cases = append(cases, fmt.Appendf(nil, "token%d and some shared filler text for balance", i))
	}
	for _, c := range cases {
		if got, want := simhash(c), simhashRef(c); got != want {
			t.Errorf("simhash(%.40q...) = %#x, reference %#x", c, got, want)
		}
	}
}

func BenchmarkSimhash(b *testing.B) {
	// ~7.7 KiB of word-like text, the real CC page size the importer
	// lab measured (server3, labs/c2/03_importer).
	var page []byte
	for i := 0; len(page) < 7700; i++ {
		page = fmt.Appendf(page, "word%d lorem ipsum dolor sit amet ", i)
	}
	b.SetBytes(int64(len(page)))
	for b.Loop() {
		simhash(page)
	}
}
