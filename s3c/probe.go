package s3c

import (
	"context"
	"errors"
	"fmt"
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
	etag, err := c.CreateExclusive(ctx, key, []byte("chizu conditional-write probe"))
	if err != nil {
		return false, fmt.Errorf("s3c: probe create: %w", err)
	}
	defer func() { _ = c.Delete(ctx, key) }()

	_, err = c.CreateExclusive(ctx, key, []byte("must lose"))
	if err == nil {
		return false, errors.New("s3c: bucket ignores If-None-Match; the chain cannot run here")
	}
	if !errors.Is(err, ErrPrecondition) {
		return false, fmt.Errorf("s3c: probe re-create: %w", err)
	}

	_, err = c.ReplaceIfMatch(ctx, key, []byte("must lose"), "chizu-bogus-etag")
	if err == nil {
		// The stale-etag replace landed: If-Match is ignored here.
		return false, nil
	}
	if !errors.Is(err, ErrPrecondition) {
		return false, fmt.Errorf("s3c: probe stale replace: %w", err)
	}
	_, err = c.ReplaceIfMatch(ctx, key, []byte("must win"), etag)
	if errors.Is(err, ErrPrecondition) {
		// Refusing the current etag too means If-Match is not coherent.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("s3c: probe fresh replace: %w", err)
	}
	return true, nil
}
