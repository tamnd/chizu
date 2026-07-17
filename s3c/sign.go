package s3c

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// emptyPayloadHash is sha256 of zero bytes, the payload hash for bodyless requests.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds SigV4 authentication to req in place. payloadHash is the lowercase
// hex sha256 of the request body. Every header already set on the request is
// signed along with Host, so callers set all headers first and sign last.
func sign(req *http.Request, accessKey, secretKey, region, service, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	shortDate := now.UTC().Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names := []string{"host"}
	for k := range req.Header {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		v := host
		if k != "host" {
			v = strings.TrimSpace(req.Header.Get(k))
		}
		canonHeaders.WriteString(k)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(v)
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonReq := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := shortDate + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonReq))
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), shortDate)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, toSign))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
			", SignedHeaders="+signedHeaders+
			", Signature="+signature)
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// canonicalURI returns the AWS-canonical path: every byte percent-encoded
// except unreserved characters and the path separators themselves.
func canonicalURI(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	return s3Escape(p, false)
}

// canonicalQuery sorts parameters by key then value and encodes both with the
// AWS variant (space is %20, tilde stays, everything else reserved is encoded).
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(s3Escape(k, true))
			b.WriteByte('=')
			b.WriteString(s3Escape(v, true))
		}
	}
	return b.String()
}

const upperhex = "0123456789ABCDEF"

// s3Escape percent-encodes s the way SigV4 canonicalization requires.
// encodeSlash controls whether '/' is encoded (true for query components,
// false for paths).
func s3Escape(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0xf])
		}
	}
	return b.String()
}
