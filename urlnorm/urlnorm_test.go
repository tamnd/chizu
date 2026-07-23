package urlnorm

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLawGolden(t *testing.T) {
	f, err := os.Open("testdata/law1.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		row := sc.Text()
		if row == "" || strings.HasPrefix(row, "#") {
			continue
		}
		input, want, ok := strings.Cut(row, "\t")
		if !ok {
			t.Fatalf("law1.txt:%d: no tab", line)
		}
		got, err := Canonicalize(input)
		if want == "!err" {
			if err == nil {
				t.Errorf("law1.txt:%d: %q canonicalized to %q, want rejection", line, input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("law1.txt:%d: %q rejected: %v", line, input, err)
			continue
		}
		if got != want {
			t.Errorf("law1.txt:%d: %q\n got %q\nwant %q", line, input, got, want)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

// Canonicalization must be idempotent: the canonical form of a
// canonical form is itself. Joins depend on this.
func TestIdempotent(t *testing.T) {
	f, err := os.Open("testdata/law1.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		row := sc.Text()
		if row == "" || strings.HasPrefix(row, "#") {
			continue
		}
		_, want, _ := strings.Cut(row, "\t")
		if want == "!err" {
			continue
		}
		again, err := Canonicalize(want)
		if err != nil {
			t.Fatalf("canonical %q rejected on second pass: %v", want, err)
		}
		if again != want {
			t.Errorf("not idempotent: %q -> %q", want, again)
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint("https://example.com/")
	b := Fingerprint("https://example.com/x")
	if a == b {
		t.Fatal("distinct URLs share a fingerprint")
	}
	if a != Fingerprint("https://example.com/") {
		t.Fatal("fingerprint is not a pure function")
	}
}

func TestLawVersion(t *testing.T) {
	if LawVersion != 1 {
		t.Fatalf("LawVersion %d: bumping it is a corpus event, update the golden file name and this test together", LawVersion)
	}
}

// Rejections must never panic, whatever discovery throws at them.
func TestGarbageInputs(t *testing.T) {
	for _, raw := range []string{
		"", " ", "http://", "https://:8080/", "%%%", "http://exa mple.com/",
		"https://[::1/", "http://\x00.com/", strings.Repeat("https://a.com/%zz", 200),
	} {
		if got, err := Canonicalize(raw); err == nil && !strings.Contains(got, "://") {
			t.Errorf("%q: produced %q without a scheme", raw, got)
		}
	}
}

func FuzzCanonicalize(f *testing.F) {
	f.Add("https://example.com/a/../b;jsessionid=X?utm_source=1&q=2#f")
	f.Add("HTTP://ex.com:80//x/%41?PHPSESSID=9")
	f.Add("https://münchen.de/straße?a=%c3%9f")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := Canonicalize(raw)
		if err != nil {
			return
		}
		again, err := Canonicalize(got)
		if err != nil {
			t.Fatalf("canonical %q rejected on second pass: %v", got, err)
		}
		if again != got {
			t.Fatalf("not idempotent: %q -> %q -> %q", raw, got, again)
		}
	})
}

func ExampleCanonicalize() {
	c, _ := Canonicalize("HTTP://Example.COM:80/a/../shop;jsessionid=Z9?utm_source=mail&sku=42#frag")
	fmt.Println(c)
	// Output: http://example.com/shop?sku=42
}
