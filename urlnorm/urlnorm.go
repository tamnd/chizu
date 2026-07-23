// Package urlnorm is the canonicalization law list of doc 03 section 7:
// one set of rules applied identically at discovery, fetch, redirect,
// and join time. The laws are versioned data, not drifting behavior;
// LawVersion is stamped into every page row so a law change is a
// visible corpus event, never a silent identity shift.
package urlnorm

import (
	"crypto/sha256"
	"errors"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// LawVersion is stamped into segments (PageRow.LawVer). Bump it when
// any law or law-list data changes, never for refactors.
const LawVersion = 1

var (
	ErrScheme = errors.New("urlnorm: scheme is not http or https")
	ErrHost   = errors.New("urlnorm: missing or invalid host")
)

// Canonicalize applies the law list and returns the canonical form,
// which is what .cold stores. An error means the URL is unfetchable
// under the laws and must never enter the frontier (CR-I5).
func Canonicalize(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}

	// Law 1: lowercase scheme and host, punycode the host, strip
	// default ports, drop fragments. Userinfo is dropped too: it is
	// never part of page identity and never something to refetch.
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrScheme
	}
	host := strings.ToLower(strings.TrimRight(u.Hostname(), "."))
	if host == "" {
		return "", ErrHost
	}
	if !isIPLiteral(host) {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil || !validDNSLength(host) {
			return "", ErrHost
		}
	}
	if port := u.Port(); port != "" {
		def := (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
		if !def {
			host += ":" + port
		}
	}

	// Law 2: percent-encoding normalizes first, because "%2E" is a dot
	// segment and must be one before dot segments resolve (reencode
	// never emits a raw "/", so it cannot invent segment boundaries).
	// Then law 4's jsessionid matrix segments come off, duplicate
	// slashes collapse, and dot segments resolve.
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	path = reencode(path, isPathByte)
	path = stripSessionSegments(path)
	path = removeDotSegments(collapseSlashes(path))

	// Law 3: preserve query order, strip tracking parameters; law 4
	// strips session-id parameters the same way.
	query := canonicalQuery(u.RawQuery)

	var b strings.Builder
	b.Grow(len(scheme) + len(host) + len(path) + len(query) + 4)
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	b.WriteString(path)
	if query != "" {
		b.WriteByte('?')
		b.WriteString(query)
	}
	return b.String(), nil
}

// Fingerprint is law 5: the 128-bit urlfp of the canonical form, the
// same sha256 prefix the fixture generator uses.
func Fingerprint(canonical string) [16]byte {
	s := sha256.Sum256([]byte(canonical))
	var fp [16]byte
	copy(fp[:], s[:16])
	return fp
}

// validDNSLength enforces what idna.Lookup.ToASCII does not: labels of
// 1..63 octets and 253 total. Without it a monster Unicode label
// punycodes on the first pass and rejects on the second, breaking
// idempotency.
func validDNSLength(host string) bool {
	if len(host) > 253 {
		return false
	}
	for lbl := range strings.SplitSeq(host, ".") {
		if len(lbl) == 0 || len(lbl) > 63 {
			return false
		}
	}
	return true
}

func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") {
		return true // bracketed IPv6, already validated by url.Parse
	}
	for i := 0; i < len(host); i++ {
		if c := host[i]; (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// collapseSlashes folds "//" runs in the path; the query never gets here.
func collapseSlashes(path string) string {
	if !strings.Contains(path, "//") {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	prev := byte(0)
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && prev == '/' {
			continue
		}
		b.WriteByte(path[i])
		prev = path[i]
	}
	return b.String()
}

// removeDotSegments is RFC 3986 section 5.2.4 over the escaped path.
func removeDotSegments(path string) string {
	var out []string
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	joined := strings.Join(out, "/")
	if !strings.HasPrefix(joined, "/") {
		joined = "/" + joined
	}
	// A trailing "." or ".." segment leaves a directory reference.
	if strings.HasSuffix(path, "/.") || strings.HasSuffix(path, "/..") {
		if !strings.HasSuffix(joined, "/") {
			joined += "/"
		}
	}
	return joined
}

// stripSessionSegments removes ";jsessionid=..." matrix segments from
// the path (law 4), case-insensitively, before any re-encoding.
func stripSessionSegments(path string) string {
	lower := strings.ToLower(path)
	for {
		i := strings.Index(lower, ";jsessionid=")
		if i < 0 {
			return path
		}
		end := i + len(";jsessionid=")
		for end < len(path) && path[end] != '/' && path[end] != ';' && path[end] != '?' {
			end++
		}
		path = path[:i] + path[end:]
		lower = lower[:i] + lower[end:]
	}
}

// canonicalQuery re-encodes each key=value pair in order and drops
// tracking and session parameters. Order is preserved because
// reordering changes semantics on enough sites to matter (law 3).
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var kept []string
	for part := range strings.SplitSeq(rawQuery, "&") {
		if part == "" {
			continue
		}
		key, _, _ := strings.Cut(part, "=")
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if strippedParam(strings.ToLower(key)) {
			continue
		}
		kept = append(kept, reencode(part, isQueryByte))
	}
	return strings.Join(kept, "&")
}

const upperhex = "0123456789ABCDEF"

// reencode normalizes percent-encoding: unreserved octets become
// literal, everything else the byte class disallows becomes uppercase
// %XX, and existing escapes are kept but canonicalized (law 2).
func reencode(s string, allowed func(byte) bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			hi, okh := unhex(s[i+1])
			lo, okl := unhex(s[i+2])
			if okh && okl {
				oct := hi<<4 | lo
				if isUnreserved(oct) {
					b.WriteByte(oct)
				} else {
					b.WriteByte('%')
					b.WriteByte(upperhex[oct>>4])
					b.WriteByte(upperhex[oct&15])
				}
				i += 2
				continue
			}
		}
		if allowed(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&15])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func isUnreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// isPathByte: pchar plus "/" (RFC 3986), the bytes a path keeps literal.
func isPathByte(c byte) bool {
	if isUnreserved(c) {
		return true
	}
	switch c {
	case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@', '/':
		return true
	}
	return false
}

// isQueryByte: pchar plus "/?", and "%" never reaches here unescaped.
func isQueryByte(c byte) bool {
	return isPathByte(c) || c == '?'
}
