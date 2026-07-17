package s3c

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// CompletedPart names one uploaded part for MultipartComplete.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

type initiateResult struct {
	UploadID string `xml:"UploadId"`
}

type completeRequest struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeResult struct {
	XMLName xml.Name
	ETag    string `xml:"ETag"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// MultipartCreate starts a multipart upload and returns its upload id.
func (c *Client) MultipartCreate(ctx context.Context, key string) (string, error) {
	q := url.Values{}
	q.Set("uploads", "")
	// Initiation writes nothing visible; a duplicate initiation just makes an
	// orphan upload id that abort or bucket lifecycle cleans up.
	r, err := c.do(ctx, req{method: http.MethodPost, key: key, query: q, idempotent: true})
	if err != nil {
		return "", err
	}
	var res initiateResult
	if err := xml.Unmarshal(r.body, &res); err != nil {
		return "", fmt.Errorf("s3c: initiate multipart: %w", err)
	}
	if res.UploadID == "" {
		return "", errors.New("s3c: initiate multipart: empty upload id")
	}
	return res.UploadID, nil
}

// MultipartPut uploads one part (1-based part numbers). Re-uploading the same
// part number replaces it, so transport retries are safe.
func (c *Client) MultipartPut(ctx context.Context, key, uploadID string, partNumber int, data []byte) (string, error) {
	q := url.Values{}
	q.Set("partNumber", strconv.Itoa(partNumber))
	q.Set("uploadId", uploadID)
	r, err := c.do(ctx, req{method: http.MethodPut, key: key, query: q, body: data, idempotent: true})
	if err != nil {
		return "", err
	}
	return etagOf(r.header), nil
}

// MultipartComplete finishes the upload; this is the atomicity point, the
// object is invisible until it succeeds. exclusive adds If-None-Match: * so
// completion is a CAS-create (ErrPrecondition means the key already exists).
func (c *Client) MultipartComplete(ctx context.Context, key, uploadID string, parts []CompletedPart, exclusive bool) (string, error) {
	cr := completeRequest{Parts: make([]completePart, len(parts))}
	for i, p := range parts {
		cr.Parts[i] = completePart{PartNumber: p.PartNumber, ETag: `"` + p.ETag + `"`}
	}
	body, err := xml.Marshal(cr)
	if err != nil {
		return "", err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/xml")
	if exclusive {
		h.Set("If-None-Match", "*")
	}
	q := url.Values{}
	q.Set("uploadId", uploadID)
	r, err := c.do(ctx, req{method: http.MethodPost, key: key, query: q, header: h, body: body})
	if err != nil {
		return "", err
	}
	// S3's classic quirk: completion can return 200 with an error document in
	// the body. Detect it by the root element name.
	var res completeResult
	if err := xml.Unmarshal(r.body, &res); err != nil {
		return "", fmt.Errorf("s3c: complete multipart: %w", err)
	}
	if res.XMLName.Local == "Error" {
		return "", &APIError{Status: r.status, Code: res.Code, Message: res.Message}
	}
	etag := res.ETag
	if len(etag) >= 2 && etag[0] == '"' {
		etag = etag[1 : len(etag)-1]
	}
	return etag, nil
}

// MultipartAbort abandons the upload and frees its stored parts.
func (c *Client) MultipartAbort(ctx context.Context, key, uploadID string) error {
	q := url.Values{}
	q.Set("uploadId", uploadID)
	_, err := c.do(ctx, req{method: http.MethodDelete, key: key, query: q, idempotent: true})
	return err
}

// Upload streams r to key as a multipart upload with parallel part puts.
// partSize must be at least the S3 5 MiB minimum for every part but the last;
// concurrency bounds in-flight parts (and so memory: concurrency x partSize).
// exclusive makes the final completion a CAS-create. The upload is aborted on
// any failure, so a failed Upload leaves no visible object and no stored parts.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, partSize int64, concurrency int, exclusive bool) (string, error) {
	if partSize < 5<<20 {
		return "", errors.New("s3c: part size below the 5 MiB S3 minimum")
	}
	if concurrency < 1 {
		concurrency = 1
	}
	uploadID, err := c.MultipartCreate(ctx, key)
	if err != nil {
		return "", err
	}

	uctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type done struct {
		part CompletedPart
		err  error
	}
	sem := make(chan struct{}, concurrency)
	results := make(chan done)
	var wg sync.WaitGroup

	var uploadErr error
	parts := []CompletedPart{}
	collect := func(d done) {
		if d.err != nil {
			if uploadErr == nil {
				uploadErr = d.err
				cancel()
			}
			return
		}
		parts = append(parts, d.part)
	}

	go func() {
		num := 0
	produce:
		for {
			buf := make([]byte, partSize)
			n, rerr := io.ReadFull(r, buf)
			if n > 0 {
				num++
				pn := num
				data := buf[:n]
				select {
				case sem <- struct{}{}:
				case <-uctx.Done():
					results <- done{err: uctx.Err()}
					break produce
				}
				wg.Go(func() {
					defer func() { <-sem }()
					etag, perr := c.MultipartPut(uctx, key, uploadID, pn, data)
					results <- done{part: CompletedPart{PartNumber: pn, ETag: etag}, err: perr}
				})
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
					results <- done{err: rerr}
				}
				break produce
			}
		}
		wg.Wait()
		close(results)
	}()

	for d := range results {
		collect(d)
	}
	if uploadErr != nil {
		// Best-effort abort with a fresh context: uctx is already canceled.
		_ = c.MultipartAbort(context.WithoutCancel(ctx), key, uploadID)
		return "", uploadErr
	}
	if len(parts) == 0 {
		_ = c.MultipartAbort(ctx, key, uploadID)
		return "", errors.New("s3c: empty upload")
	}
	// Part goroutines finish out of order; completion needs part order.
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j].PartNumber < parts[j-1].PartNumber; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	etag, err := c.MultipartComplete(ctx, key, uploadID, parts, exclusive)
	if err != nil {
		if !errors.Is(err, ErrPrecondition) {
			_ = c.MultipartAbort(context.WithoutCancel(ctx), key, uploadID)
		} else {
			_ = c.MultipartAbort(ctx, key, uploadID)
		}
		return "", err
	}
	return etag, nil
}
