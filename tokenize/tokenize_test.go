package tokenize

import (
	"strings"
	"testing"
)

type tok struct {
	term string
	pos  uint32
}

func collect(t *testing.T, text string) ([]tok, Stats) {
	t.Helper()
	var st Stats
	var out []tok
	Text(text, &st, func(term []byte, pos uint32) {
		out = append(out, tok{string(term), pos})
	})
	return out, st
}

func terms(toks []tok) []string {
	s := make([]string, len(toks))
	for i, tk := range toks {
		s[i] = tk.term
	}
	return s
}

func TestBasicWords(t *testing.T) {
	got, st := collect(t, "Hello, World! It's 2026.")
	want := []tok{{"hello", 0}, {"world", 1}, {"it", 2}, {"s", 3}, {"2026", 4}}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d: got %v want %v", i, got[i], want[i])
		}
	}
	if st[Word] != 4 || st[Number] != 1 {
		t.Errorf("histogram: %d words %d numbers", st[Word], st[Number])
	}
}

func TestNFKCFold(t *testing.T) {
	// Fullwidth forms and the fi ligature normalize before segmentation.
	got, _ := collect(t, "Ｈｅｌｌｏ １２３ ﬁle")
	want := []string{"hello", "123", "file"}
	if strings.Join(terms(got), " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", terms(got), want)
	}
}

func TestCJKBigrams(t *testing.T) {
	got, st := collect(t, "日本語")
	want := []tok{{"日本", 0}, {"本語", 1}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
	if st[CJKBigram] != 2 {
		t.Errorf("bigram count %d", st[CJKBigram])
	}
}

func TestCJKUnigramAndMixed(t *testing.T) {
	got, st := collect(t, "the 中 word")
	want := []string{"the", "中", "word"}
	if strings.Join(terms(got), " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", terms(got), want)
	}
	if st[CJKUnigram] != 1 {
		t.Errorf("unigram count %d", st[CJKUnigram])
	}
	// Kana bigrams the same way as Han.
	got, _ = collect(t, "カタカナ")
	if len(got) != 3 || got[0].term != "カタ" {
		t.Fatalf("kana: got %v", got)
	}
}

func TestNumberAdmission(t *testing.T) {
	sixteen := strings.Repeat("7", 16)
	seventeen := strings.Repeat("7", 17)
	got, st := collect(t, sixteen+" "+seventeen)
	if len(got) != 1 || got[0].term != sixteen {
		t.Fatalf("got %v", got)
	}
	if st[Number] != 1 || st[DropLongNumber] != 1 {
		t.Errorf("histogram: number %d long %d", st[Number], st[DropLongNumber])
	}
}

func TestLengthAdmission(t *testing.T) {
	ok := strings.Repeat("ab", 32)         // 64 bytes, admitted
	long := strings.Repeat("ab", 32) + "c" // 65 bytes, dropped
	got, st := collect(t, ok+" "+long)
	if len(got) != 1 || got[0].term != ok {
		t.Fatalf("got %d terms", len(got))
	}
	if st[DropTooLong] != 1 {
		t.Errorf("too-long count %d", st[DropTooLong])
	}
}

func TestJunkClasses(t *testing.T) {
	base64ish := "dGhlcXVpY2ticm93bjZveDE3" // 24 alnum, letters+digits
	repeat := "aaaaaaaaaa"
	hexid := "u3f9c2d1e8a7b6054aa" // 19 bytes, id-like, admitted
	got, st := collect(t, base64ish+" "+repeat+" "+hexid)
	if len(got) != 1 || got[0].term != hexid {
		t.Fatalf("got %v", terms(got))
	}
	if st[DropBase64] != 1 || st[DropRepeat] != 1 || st[Word] != 1 {
		t.Errorf("histogram: base64 %d repeat %d word %d", st[DropBase64], st[DropRepeat], st[Word])
	}
}

func TestBinaryRun(t *testing.T) {
	got, st := collect(t, "good \xff\xfe\xfd text")
	want := []tok{{"good", 0}, {"text", 2}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
	if st[DropBinary] != 1 {
		t.Errorf("binary run counted %d times, want 1", st[DropBinary])
	}
}

func TestPositionsGapOnDrop(t *testing.T) {
	// Dropped tokens keep their position slot so phrase gaps survive.
	got, _ := collect(t, "alpha "+strings.Repeat("z", 70)+" beta")
	if len(got) != 2 || got[0].pos != 0 || got[1].pos != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestDeterministic(t *testing.T) {
	text := "The 日本語 quick ﬁx 12345 " + strings.Repeat("x1f9", 8)
	a, sa := collect(t, text)
	b, sb := collect(t, text)
	if len(a) != len(b) || sa != sb {
		t.Fatalf("nondeterministic: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("token %d differs: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestClassNames(t *testing.T) {
	for c := Class(0); c < numClasses; c++ {
		if c.String() == "" {
			t.Errorf("class %d has no name", c)
		}
	}
	if !Word.Admitted() || DropTooLong.Admitted() {
		t.Error("admission split wrong")
	}
}

func TestStatsAdd(t *testing.T) {
	var a, b Stats
	a[Word] = 3
	b[Word] = 4
	b[DropRepeat] = 1
	a.Add(&b)
	if a[Word] != 7 || a[DropRepeat] != 1 {
		t.Errorf("add: %v", a)
	}
}
