package s3c

import (
	"net/http"
	"testing"
	"time"
)

// The four published AWS SigV4 S3 examples (the AKIAIOSFODNN7EXAMPLE set,
// 2013-05-24, us-east-1). They pin the whole signer hermetically: canonical
// paths with special characters, empty-value and multi-parameter query
// strings, extra signed headers, and a non-empty payload hash.
func TestSignAWSVectors(t *testing.T) {
	const (
		access = "AKIAIOSFODNN7EXAMPLE"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		// sha256 of "Welcome to Amazon S3."
		welcomeHash = "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072"
	)
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		method  string
		url     string
		headers map[string]string
		payload string
		want    string
	}{
		{
			name:    "get object",
			method:  http.MethodGet,
			url:     "https://examplebucket.s3.amazonaws.com/test.txt",
			headers: map[string]string{"Range": "bytes=0-9"},
			payload: emptyPayloadHash,
			want:    "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "put object",
			method: http.MethodPut,
			url:    "https://examplebucket.s3.amazonaws.com/test$file.text",
			headers: map[string]string{
				"Date":                "Fri, 24 May 2013 00:00:00 GMT",
				"x-amz-storage-class": "REDUCED_REDUNDANCY",
			},
			payload: welcomeHash,
			want:    "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name:    "get bucket lifecycle",
			method:  http.MethodGet,
			url:     "https://examplebucket.s3.amazonaws.com?lifecycle=",
			payload: emptyPayloadHash,
			want:    "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name:    "list objects",
			method:  http.MethodGet,
			url:     "https://examplebucket.s3.amazonaws.com?max-keys=2&prefix=J",
			payload: emptyPayloadHash,
			want:    "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			sign(req, access, secret, "us-east-1", "s3", tc.payload, at)
			auth := req.Header.Get("Authorization")
			wantSuffix := "Signature=" + tc.want
			if len(auth) < len(wantSuffix) || auth[len(auth)-len(wantSuffix):] != wantSuffix {
				t.Fatalf("authorization mismatch:\n got %s\nwant suffix %s", auth, wantSuffix)
			}
		})
	}
}

func TestS3Escape(t *testing.T) {
	if got := s3Escape("/a b/c$~d", false); got != "/a%20b/c%24~d" {
		t.Fatalf("path escape: got %q", got)
	}
	if got := s3Escape("a/b c", true); got != "a%2Fb%20c" {
		t.Fatalf("query escape: got %q", got)
	}
}
