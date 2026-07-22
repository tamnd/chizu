package fixture

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"unicode/utf8"
)

// Term spelling: rank in, byte string out, unique by construction
// because the four classes use disjoint alphabets (letters only, digits
// only, x-prefixed hex, CJK runes). The class mix follows the doc 06
// admission picture: words dominate, with number, id, and CJK minorities.
//
// The top 1024 ranks are always words so the corpus head reads like
// language; past that, rank mod 20 deals 14 words, 3 numbers, 2 ids,
// 1 CJK.

var (
	termOnsets = []string{"b", "c", "d", "f", "g", "h", "j", "k", "l", "m", "n", "p", "r", "s", "t", "v", "w", "z", "ch", "sh", "st", "tr", "pl", "br", "gr", "sp"}
	termVowels = []string{"a", "e", "i", "o", "u", "ai", "ea", "ou", "io"}
)

// appendTerm spells rank into dst. In CJK mode every rank spells as
// CJK, which is how CJK-language docs stay CJK throughout.
func appendTerm(dst []byte, rank uint64, cjk bool) []byte {
	if cjk {
		return appendCJK(dst, rank)
	}
	if rank >= 1024 {
		switch rank % 20 {
		case 14, 15, 16:
			return fmt.Appendf(dst, "%d", rank)
		case 17, 18:
			return fmt.Appendf(dst, "x%x", splitmix(rank))
		case 19:
			return appendCJK(dst, rank)
		}
	}
	return appendWord(dst, rank)
}

// appendWord is a base-234 syllable numeral system (26 onsets times 9
// vowels): unique letters-only spellings, short for small ranks.
func appendWord(dst []byte, rank uint64) []byte {
	for {
		d := rank % 234
		dst = append(dst, termOnsets[d%26]...)
		dst = append(dst, termVowels[d/26]...)
		rank /= 234
		if rank == 0 {
			return dst
		}
		rank--
	}
}

// appendCJK spells rank in base 0x1500 over the unified-ideograph run
// starting at U+4E00, two runes minimum so terms look like bigram
// output.
func appendCJK(dst []byte, rank uint64) []byte {
	n := 0
	for {
		dst = utf8.AppendRune(dst, rune(0x4E00+rank%0x1500))
		rank /= 0x1500
		n++
		if rank == 0 && n >= 2 {
			return dst
		}
		if rank > 0 {
			rank--
		}
	}
}

// tailTerm is the doc-unique id term: u-prefixed hex keyed by (seed,
// docid), df 1 across the corpus up to hash-collision noise.
func tailTerm(seed, docid uint64) string {
	return fmt.Sprintf("u%016x", splitmix(seed^splitmix(docid+0xC0FFEE)))
}

// spliceMemberTokens turns a template body into a member body: one
// member token spliced in every mutationEvery tokens, which moves a
// handful of simhash bits and no more.
const mutationEvery = 50

func spliceMemberTokens(text string, r *rand.Rand, docid uint64) string {
	words := strings.Split(text, " ")
	member := tailTerm(uint64(r.Int63()), docid)
	var b strings.Builder
	b.Grow(len(text) + 64)
	spliced := false
	for i, w := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		if i%mutationEvery == mutationEvery-1 {
			b.WriteString(member)
			b.WriteByte(' ')
			spliced = true
		}
		b.WriteString(w)
	}
	// A template shorter than mutationEvery still has to differ per
	// member, or the cluster degenerates to exact dups.
	if !spliced {
		b.WriteByte(' ')
		b.WriteString(member)
	}
	return b.String()
}

// simhash is the classic 64-bit Charikar sketch over whitespace tokens
// with FNV-1a features, the same shape the doc 06 near-dup pass will
// compute; template members land within a few bits of each other.
func simhash(text string) uint64 {
	var acc [64]int32
	for tok := range strings.FieldsSeq(text) {
		h := fnv.New64a()
		h.Write([]byte(tok))
		f := h.Sum64()
		for b := range 64 {
			if f&(1<<b) != 0 {
				acc[b]++
			} else {
				acc[b]--
			}
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
