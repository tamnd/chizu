package s3c

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// deleteBatchMax is the S3 per-request key limit for DeleteObjects.
const deleteBatchMax = 1000

type deleteRequest struct {
	XMLName xml.Name       `xml:"Delete"`
	Quiet   bool           `xml:"Quiet"`
	Objects []deleteObject `xml:"Object"`
}

type deleteObject struct {
	Key string `xml:"Key"`
}

type deleteResult struct {
	Errors []struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// DeleteBatch removes the keys with DeleteObjects requests, 1000 keys per
// call. Missing keys delete successfully, per S3; any per-key error fails the
// whole call with every failed key named.
func (c *Client) DeleteBatch(ctx context.Context, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), deleteBatchMax)
		chunk, rest := keys[:n], keys[n:]

		dr := deleteRequest{Quiet: true, Objects: make([]deleteObject, n)}
		for i, k := range chunk {
			dr.Objects[i] = deleteObject{Key: k}
		}
		body, err := xml.Marshal(dr)
		if err != nil {
			return err
		}
		sum := md5.Sum(body)
		h := http.Header{}
		h.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
		h.Set("Content-Type", "application/xml")

		q := url.Values{}
		q.Set("delete", "")
		// The request replays byte-identically and a re-delete of an already
		// deleted key succeeds, so DeleteObjects is idempotent despite being
		// a POST.
		r, err := c.do(ctx, req{method: http.MethodPost, query: q, header: h, body: body, idempotent: true})
		if err != nil {
			return err
		}
		var res deleteResult
		if err := xml.Unmarshal(r.body, &res); err == nil && len(res.Errors) > 0 {
			var b strings.Builder
			for i, e := range res.Errors {
				if i > 0 {
					b.WriteString("; ")
				}
				fmt.Fprintf(&b, "%s: %s %s", e.Key, e.Code, e.Message)
			}
			return fmt.Errorf("s3c: delete batch: %s", b.String())
		}
		keys = rest
	}
	return nil
}
