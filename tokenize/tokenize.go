// Package tokenize is the versioned text tokenizer of doc 06 section 3:
// NFKC fold, lowercase, no stemming, CJK as overlapping bigrams, numbers
// kept up to 16 digits, term admission 1..64 bytes with the junk classes
// (binary runs, base64-looking, repeated-char runs) dropped.
//
// The version freezes per index generation: a shard built with version N
// must query with version N, and any change to emitted terms bumps it.
// Admission is a resident-RAM decision disguised as a text-processing
// detail (the doc 05 dictionary line assumes admission cuts the raw
// vocabulary), so every call feeds the admission-class histogram and the
// corpus-stats suite reports it per generation.
package tokenize

import (
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Version freezes the tokenizer. Any change to emitted terms bumps it.
const Version = 1

// Admission constants from doc 06 section 3.
const (
	maxTermBytes = 64
	maxNumberLen = 16
	// base64Min is the shortest all-alphanumeric letters-plus-digits
	// token treated as machine garbage. Real session ids and hashes run
	// 32 bytes and up; 24 leaves headroom while passing shorter id-like
	// terms (a 17-byte hex id is a legitimate df-1 term).
	base64Min = 24
	// repeatMin is the shortest same-rune run that marks a token as a
	// repeated-char run.
	repeatMin = 8
)

// Class is one row of the admission histogram. Classes below dropTooLong
// are admitted; the rest are dropped.
type Class uint8

const (
	Word Class = iota
	Number
	CJKBigram
	CJKUnigram
	DropTooLong
	DropLongNumber
	DropBinary
	DropBase64
	DropRepeat
	numClasses
)

var classNames = [numClasses]string{
	"word", "number", "cjk-bigram", "cjk-unigram",
	"drop-too-long", "drop-long-number", "drop-binary", "drop-base64", "drop-repeat",
}

func (c Class) String() string { return classNames[c] }

// Admitted reports whether terms of this class enter the index.
func (c Class) Admitted() bool { return c < DropTooLong }

// Stats is the admission-class histogram; one per build pass, added into
// from every Text call that carries it.
type Stats [numClasses]uint64

// Add merges another histogram in.
func (s *Stats) Add(o *Stats) {
	for i := range s {
		s[i] += o[i]
	}
}

// Text tokenizes one document's text and emits every admitted term with
// its position. The term slice is reused across calls; the callee copies
// what it keeps. Positions advance for dropped tokens too, so phrase
// gaps survive admission.
func Text(text string, st *Stats, emit func(term []byte, pos uint32)) {
	if !norm.NFKC.IsNormalString(text) {
		text = norm.NFKC.String(text)
	}

	var (
		pos    uint32
		term   []byte
		cjk    []rune
		binRun bool
	)
	var gram []byte
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			// A lone CJK rune between other scripts indexes as itself;
			// there is no partner to bigram with.
			gram = utf8.AppendRune(gram[:0], cjk[0])
			st[CJKUnigram]++
			emit(gram, pos)
			pos++
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				gram = utf8.AppendRune(gram[:0], cjk[i])
				gram = utf8.AppendRune(gram, cjk[i+1])
				st[CJKBigram]++
				emit(gram, pos)
				pos++
			}
		}
		cjk = cjk[:0]
	}
	flushTerm := func() {
		if len(term) == 0 {
			return
		}
		cl := admit(term)
		st[cl]++
		if cl.Admitted() {
			emit(term, pos)
		}
		pos++
		term = term[:0]
	}

	for _, r := range text {
		switch {
		case isCJK(r):
			flushTerm()
			cjk = append(cjk, r)
		case r == utf8.RuneError:
			// Binary garbage that survived into stored text; a whole
			// run counts as one dropped token so the histogram sees
			// documents, not bytes.
			flushTerm()
			flushCJK()
			if !binRun {
				st[DropBinary]++
				pos++
				binRun = true
			}
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r):
			flushCJK()
			term = utf8.AppendRune(term, unicode.ToLower(r))
		default:
			flushTerm()
			flushCJK()
		}
		binRun = false
	}
	flushTerm()
	flushCJK()
}

// admit classifies one folded token. Order matters: length caps first,
// then the junk classes, then the survivors.
func admit(term []byte) Class {
	digits, letters := 0, 0
	runes := 0
	var prev rune
	run, maxRun := 0, 0
	for i := 0; i < len(term); {
		r, sz := utf8.DecodeRune(term[i:])
		i += sz
		runes++
		switch {
		case r >= '0' && r <= '9':
			digits++
		case unicode.IsLetter(r):
			letters++
		}
		if r == prev {
			run++
		} else {
			run = 1
			prev = r
		}
		maxRun = max(maxRun, run)
	}
	switch {
	case digits == runes && letters == 0:
		if runes > maxNumberLen {
			return DropLongNumber
		}
		return Number
	case len(term) > maxTermBytes:
		return DropTooLong
	case maxRun >= repeatMin:
		return DropRepeat
	case runes >= base64Min && digits > 0 && letters > 0 && digits+letters == runes:
		return DropBase64
	}
	return Word
}

// isCJK reports whether r indexes as bigram units: Han plus the kana.
// Hangul is alphabetic and space-delimited, so it rides the word path.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
}
