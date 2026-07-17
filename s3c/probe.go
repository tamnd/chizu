package s3c

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// The probe payloads are pairwise distinct so that reading the key back
// after a transport failure says exactly which write landed.
var (
	probeCreatePayload = []byte("chizu conditional-write probe")
	probeLosePayload   = []byte("must lose")
	probeWinPayload    = []byte("must win")
)

// ProbeConditionalWrites verifies that the bucket honors the conditional
// writes chizu stands on, using one transient object at key.
//
// CAS-create (If-None-Match: *) is mandatory: the chain cannot exist without
// it, so a bucket that ignores it is an error. CAS-replace (If-Match) is
// optional: the returned flag reports whether it is honored, and a false
// means the root must use the rootv/ dense-sequence fallback (doc 04
// section 8). Run this before creating a database, never on a hot path.
func (c *Client) ProbeConditionalWrites(ctx context.Context, key string) (ifMatch bool, err error) {
	// A crashed earlier probe may have left the key behind.
	if err := c.Delete(ctx, key); err != nil {
		return false, fmt.Errorf("s3c: probe cleanup: %w", err)
	}
	etag, err := c.probePut(ctx, key, probeCreatePayload, func() (string, error) {
		return c.CreateExclusive(ctx, key, probeCreatePayload)
	})
	if err != nil {
		return false, fmt.Errorf("s3c: probe create: %w", err)
	}
	defer func() { _ = c.Delete(ctx, key) }()

	_, err = c.probePut(ctx, key, probeLosePayload, func() (string, error) {
		return c.CreateExclusive(ctx, key, probeLosePayload)
	})
	if err == nil {
		return false, errors.New("s3c: bucket ignores If-None-Match; the chain cannot run here")
	}
	if !errors.Is(err, ErrPrecondition) {
		return false, fmt.Errorf("s3c: probe re-create: %w", err)
	}

	_, err = c.probePut(ctx, key, probeLosePayload, func() (string, error) {
		return c.ReplaceIfMatch(ctx, key, probeLosePayload, "chizu-bogus-etag")
	})
	if err == nil {
		// The stale-etag replace landed: If-Match is ignored here.
		return false, nil
	}
	if !errors.Is(err, ErrPrecondition) {
		return false, fmt.Errorf("s3c: probe stale replace: %w", err)
	}
	_, err = c.probePut(ctx, key, probeWinPayload, func() (string, error) {
		return c.ReplaceIfMatch(ctx, key, probeWinPayload, etag)
	})
	if errors.Is(err, ErrPrecondition) {
		// Refusing the current etag too means If-Match is not coherent.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("s3c: probe fresh replace: %w", err)
	}
	return true, nil
}

// probePut runs one conditional probe write, resolving transport ambiguity
// that a hot path never could: the probe key has a single writer and every
// step's payload is distinct, so reading the key back says whether an
// ambiguous write landed. MinIO in particular may drop the connection right
// after answering a conditional request, which surfaces as an EOF on the
// next write riding the same connection.
func (c *Client) probePut(ctx context.Context, key string, payload []byte, put func() (string, error)) (etag string, err error) {
	for range 3 {
		etag, err = put()
		var amb *AmbiguousError
		if !errors.As(err, &amb) {
			return etag, err
		}
		body, gtag, gerr := c.Get(ctx, key)
		if gerr == nil && bytes.Equal(body, payload) {
			// The write reached the bucket; only the response was lost.
			return gtag, nil
		}
		if gerr != nil && !errors.Is(gerr, ErrNotFound) {
			return "", err
		}
		// The write did not land: it either never reached the bucket or
		// was refused as the connection died. Replaying the identical
		// conditional request is safe in both cases.
	}
	return etag, err
}
