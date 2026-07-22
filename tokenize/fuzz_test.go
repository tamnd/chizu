package tokenize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzText holds the tokenizer's contract on arbitrary bytes: no panic,
// every emitted term is valid UTF-8 within admission bounds, positions
// never decrease, and the run is deterministic.
func FuzzText(f *testing.F) {
	f.Add("Hello, World! 2026")
	f.Add("日本語のテキスト and latin")
	f.Add("\xff\xfe binary \x00 runs")
	f.Add(strings.Repeat("a", 100) + " " + strings.Repeat("9", 40))
	f.Add("Ｆｕｌｌｗｉｄｔｈ ﬁ ﬂ ⅷ ①")
	f.Fuzz(func(t *testing.T, text string) {
		var st Stats
		var lastPos uint32
		var first []string
		Text(text, &st, func(term []byte, pos uint32) {
			if len(term) == 0 || len(term) > maxTermBytes {
				t.Fatalf("term length %d out of bounds: %q", len(term), term)
			}
			if !utf8.Valid(term) {
				t.Fatalf("invalid UTF-8 term: %q", term)
			}
			if pos < lastPos {
				t.Fatalf("position went backward: %d after %d", pos, lastPos)
			}
			lastPos = pos
			first = append(first, string(term))
		})
		var st2 Stats
		i := 0
		Text(text, &st2, func(term []byte, pos uint32) {
			if i >= len(first) || first[i] != string(term) {
				t.Fatalf("nondeterministic at token %d", i)
			}
			i++
		})
		if i != len(first) || st != st2 {
			t.Fatal("nondeterministic token count or histogram")
		}
	})
}
