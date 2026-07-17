// Package s3c is chizu's only object-storage client: a minimal S3 client on
// stdlib net/http with SigV4, conditional writes, ranged reads, batch delete,
// and multipart upload. The design and the retry taxonomy come from spec 2107
// doc 02 section 3 (inherited from the 2064 obs1 design).
package s3c

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config describes one bucket on one endpoint. Zero values get defaults from New.
type Config struct {
	// Endpoint is the base URL, e.g. http://localhost:9000 for MinIO or
	// https://s3.us-east-1.amazonaws.com for AWS.
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// PathStyle addresses the bucket in the path (MinIO) instead of the host.
	PathStyle bool
	// MaxAttempts bounds tries for retryable failures (409/429/5xx and
	// transport errors on idempotent requests). Default 4.
	MaxAttempts int
	HTTPClient  *http.Client
}

// FromEnv builds a Config from CHIZU_S3_ENDPOINT, CHIZU_S3_ACCESS_KEY,
// CHIZU_S3_SECRET_KEY, CHIZU_S3_REGION, and CHIZU_S3_BUCKET. Endpoint empty
// means the environment provides no bucket (tests skip on this).
func FromEnv() Config {
	return Config{
		Endpoint:  os.Getenv("CHIZU_S3_ENDPOINT"),
		Region:    os.Getenv("CHIZU_S3_REGION"),
		Bucket:    os.Getenv("CHIZU_S3_BUCKET"),
		AccessKey: os.Getenv("CHIZU_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("CHIZU_S3_SECRET_KEY"),
		PathStyle: true,
	}
}

// Client is safe for concurrent use.
type Client struct {
	cfg  Config
	base *url.URL
}

// Info is object metadata from a HEAD.
type Info struct {
	Size int64
	ETag string
}

func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3c: endpoint required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3c: bucket required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3c: bad endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("s3c: endpoint %q needs scheme and host", cfg.Endpoint)
	}
	return &Client{cfg: cfg, base: u}, nil
}

// Bucket returns the configured bucket name.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// url builds the request URL for key ("" means the bucket itself).
func (c *Client) url(key string, query url.Values) *url.URL {
	u := *c.base
	if c.cfg.PathStyle {
		u.Path = "/" + c.cfg.Bucket
		if key != "" {
			u.Path += "/" + key
		}
	} else {
		u.Host = c.cfg.Bucket + "." + u.Host
		u.Path = "/"
		if key != "" {
			u.Path = "/" + key
		}
	}
	u.RawPath = s3Escape(u.Path, false)
	if u.RawPath == u.Path {
		u.RawPath = ""
	}
	u.RawQuery = canonicalQuery(query)
	return &u
}

// req describes one attempt-independent request; the body is a byte slice so
// every retry replays identical bytes.
type req struct {
	method string
	key    string // "" for bucket-level operations
	query  url.Values
	header http.Header
	body   []byte
	// idempotent marks requests that are safe to retry after a transport
	// error. Writes are not: their transport failures become AmbiguousError.
	idempotent bool
}

// resp is a fully-read response.
type resp struct {
	status int
	header http.Header
	body   []byte
}

func (c *Client) do(ctx context.Context, r req) (*resp, error) {
	payloadHash := emptyPayloadHash
	if len(r.body) > 0 {
		sum := sha256.Sum256(r.body)
		payloadHash = hex.EncodeToString(sum[:])
	}
	var last error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			base := 100 * time.Millisecond << (attempt - 1)
			delay := base + time.Duration(rand.Int63n(int64(base)))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		hr, err := http.NewRequestWithContext(ctx, r.method, c.url(r.key, r.query).String(), bytes.NewReader(r.body))
		if err != nil {
			return nil, err
		}
		for k, vs := range r.header {
			for _, v := range vs {
				hr.Header.Set(k, v)
			}
		}
		sign(hr, c.cfg.AccessKey, c.cfg.SecretKey, c.cfg.Region, "s3", payloadHash, time.Now())

		res, err := c.cfg.HTTPClient.Do(hr)
		if err != nil {
			if ctx.Err() != nil && !r.idempotent {
				return nil, &AmbiguousError{Op: r.method, Key: r.key, Err: err}
			}
			if ctx.Err() != nil {
				return nil, err
			}
			if r.idempotent {
				last = err
				continue
			}
			return nil, &AmbiguousError{Op: r.method, Key: r.key, Err: err}
		}
		body, rerr := io.ReadAll(res.Body)
		res.Body.Close()
		if rerr != nil {
			if r.idempotent {
				last = rerr
				continue
			}
			return nil, &AmbiguousError{Op: r.method, Key: r.key, Err: rerr}
		}
		switch {
		case res.StatusCode < 300:
			return &resp{status: res.StatusCode, header: res.Header, body: body}, nil
		case res.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case res.StatusCode == http.StatusPreconditionFailed:
			return nil, ErrPrecondition
		case res.StatusCode == http.StatusConflict,
			res.StatusCode == http.StatusTooManyRequests,
			res.StatusCode >= 500:
			// A definite rejection: the write did not land, so replaying the
			// identical bytes is safe for every operation.
			last = apiError(res.StatusCode, body)
			continue
		default:
			return nil, apiError(res.StatusCode, body)
		}
	}
	return nil, fmt.Errorf("s3c: %s %q gave up after %d attempts: %w", r.method, r.key, c.cfg.MaxAttempts, last)
}

func etagOf(h http.Header) string {
	return strings.Trim(h.Get("ETag"), `"`)
}

// Get returns the full object and its ETag.
func (c *Client) Get(ctx context.Context, key string) ([]byte, string, error) {
	r, err := c.do(ctx, req{method: http.MethodGet, key: key, idempotent: true})
	if err != nil {
		return nil, "", err
	}
	return r.body, etagOf(r.header), nil
}

// GetRange returns n bytes starting at off. Reading past the end returns the
// available suffix; a start past the end returns an error from the service.
func (c *Client) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	r, err := c.do(ctx, req{method: http.MethodGet, key: key, header: h, idempotent: true})
	if err != nil {
		return nil, err
	}
	return r.body, nil
}

// Head returns object metadata.
func (c *Client) Head(ctx context.Context, key string) (Info, error) {
	r, err := c.do(ctx, req{method: http.MethodHead, key: key, idempotent: true})
	if err != nil {
		return Info{}, err
	}
	size, _ := strconv.ParseInt(r.header.Get("Content-Length"), 10, 64)
	return Info{Size: size, ETag: etagOf(r.header)}, nil
}

// Put writes the object unconditionally and returns its ETag.
func (c *Client) Put(ctx context.Context, key string, data []byte) (string, error) {
	r, err := c.do(ctx, req{method: http.MethodPut, key: key, body: data})
	if err != nil {
		return "", err
	}
	return etagOf(r.header), nil
}

// CreateExclusive writes the object only if the key does not exist
// (If-None-Match: *). ErrPrecondition means someone else's object is there:
// the caller lost the race and re-reads to learn who won.
func (c *Client) CreateExclusive(ctx context.Context, key string, data []byte) (string, error) {
	h := http.Header{}
	h.Set("If-None-Match", "*")
	r, err := c.do(ctx, req{method: http.MethodPut, key: key, header: h, body: data})
	if err != nil {
		return "", err
	}
	return etagOf(r.header), nil
}

// ReplaceIfMatch replaces the object only if its current ETag still matches
// (If-Match). ErrPrecondition means the object changed under the caller.
func (c *Client) ReplaceIfMatch(ctx context.Context, key string, data []byte, etag string) (string, error) {
	h := http.Header{}
	h.Set("If-Match", `"`+etag+`"`)
	r, err := c.do(ctx, req{method: http.MethodPut, key: key, header: h, body: data})
	if err != nil {
		return "", err
	}
	return etagOf(r.header), nil
}

// Delete removes one object. Deleting a missing key succeeds, per S3.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.do(ctx, req{method: http.MethodDelete, key: key, idempotent: true})
	return err
}

// CreateBucket creates the configured bucket; a bucket that already exists
// and is owned by the caller is success.
func (c *Client) CreateBucket(ctx context.Context) error {
	// HEAD first: the exists case answers in one round trip instead of
	// burning the 409 retry budget on BucketAlreadyOwnedByYou.
	if _, err := c.do(ctx, req{method: http.MethodHead, idempotent: true}); err == nil {
		return nil
	}
	_, err := c.do(ctx, req{method: http.MethodPut, idempotent: true})
	var ae *APIError
	if errors.As(err, &ae) && (ae.Code == "BucketAlreadyOwnedByYou" || ae.Code == "BucketAlreadyExists") {
		return nil
	}
	return err
}
