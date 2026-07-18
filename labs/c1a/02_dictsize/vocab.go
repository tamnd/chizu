package main

import (
	"bytes"
	"math/rand"
	"sort"
)

// The fixture vocabulary, shaped like a post-admission web vocabulary
// (doc 06 section 3, doc 10 section 1): word-like stems with
// morphological variants for prefix mass, numbers, alphanumeric ids,
// and CJK bigrams. Deterministic under a seed; the real fixture
// generator (a later C1a slice) will reuse this shape.

var (
	vocabOnsets   = []string{"b", "c", "d", "f", "g", "h", "j", "k", "l", "m", "n", "p", "r", "s", "t", "v", "w", "z", "ch", "sh", "st", "tr", "pl", "br", "gr", "sp"}
	vocabVowels   = []string{"a", "e", "i", "o", "u", "ai", "ea", "ou", "io"}
	vocabCodas    = []string{"", "", "n", "r", "s", "t", "l", "m", "ng", "rd", "nt"}
	vocabSuffixes = []string{"", "s", "ed", "ing", "er", "ers", "ly", "tion", "al", "ness", "ment", "able"}
)

func vocabStem(rng *rand.Rand) string {
	n := 2 + rng.Intn(3)
	var b []byte
	for range n {
		b = append(b, vocabOnsets[rng.Intn(len(vocabOnsets))]...)
		b = append(b, vocabVowels[rng.Intn(len(vocabVowels))]...)
		b = append(b, vocabCodas[rng.Intn(len(vocabCodas))]...)
	}
	return string(b)
}

// fixtureVocab returns n distinct terms in strictly increasing byte
// order: ~60% words (stems sharing suffix variants), ~15% numbers,
// ~15% alphanumeric ids, ~10% CJK bigrams.
func fixtureVocab(rng *rand.Rand, n int) [][]byte {
	seen := make(map[string]struct{}, n+n/8)
	add := func(s string) {
		if len(s) > 0 && len(s) <= 64 {
			seen[s] = struct{}{}
		}
	}

	for len(seen) < n*60/100 {
		stem := vocabStem(rng)
		for range 1 + rng.Intn(4) {
			add(stem + vocabSuffixes[rng.Intn(len(vocabSuffixes))])
		}
	}
	digits := "0123456789"
	for len(seen) < n*75/100 {
		l := 1 + rng.Intn(8)
		if rng.Intn(3) == 0 {
			l = 4 // year-class
		}
		b := make([]byte, l)
		for i := range b {
			b[i] = digits[rng.Intn(10)]
		}
		add(string(b))
	}
	alnum := "abcdefghijklmnopqrstuvwxyz0123456789"
	for len(seen) < n*90/100 {
		l := 6 + rng.Intn(11)
		b := make([]byte, l)
		for i := range b {
			if rng.Intn(2) == 0 {
				b[i] = alnum[rng.Intn(16)] // hex-ish
			} else {
				b[i] = alnum[rng.Intn(36)]
			}
		}
		add(string(b))
	}
	for len(seen) < n {
		r1 := rune(0x4E00 + rng.Intn(0x1500))
		r2 := rune(0x4E00 + rng.Intn(0x1500))
		add(string(r1) + string(r2))
	}

	terms := make([][]byte, 0, len(seen))
	for s := range seen {
		terms = append(terms, []byte(s))
	}
	sort.Slice(terms, func(i, j int) bool { return bytes.Compare(terms[i], terms[j]) < 0 })
	return terms[:n]
}
