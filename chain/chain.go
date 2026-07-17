package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/tamnd/chizu/s3c"
)

// Options configures one chain handle.
type Options struct {
	// Prefix is the caller-chosen database prefix; batch objects land under
	// Prefix + "chain/<seq16>".
	Prefix string
	// Writer and Incarnation identify this process in every batch it appends.
	Writer      uint64
	Incarnation uint32
	// From is the first sequence to probe on open; boot passes the slot after
	// the newest checkpoint, a fresh database passes 0.
	From uint64
	// Observe, when set, sees every batch this handle reads in sequence
	// order: catch-up on open, winners of lost races, ambiguous slots that
	// went to someone else, and everything Poll finds. This is how a node's
	// view advances for free while it appends.
	Observe func(seq uint64, b *Batch)
}

// Chain is one node's handle on the chain. It is not safe for concurrent use;
// a node runs one append loop and batches everything pending into it, which
// is exactly what keeps contention low (doc 02 section 4).
type Chain struct {
	s3      *s3c.Client
	opt     Options
	next    uint64 // next unread, and therefore next appendable, sequence
	batchID uint64
}

// Open probes forward from Options.From until the first missing slot and
// returns a handle positioned at the tail. Strong consistency makes the 404
// trustworthy as "nothing newer exists yet".
func Open(ctx context.Context, client *s3c.Client, opt Options) (*Chain, error) {
	c := &Chain{s3: client, opt: opt, next: opt.From}
	if err := c.Poll(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Pos returns the next sequence this handle would read or contend for.
func (c *Chain) Pos() uint64 { return c.next }

func (c *Chain) key(seq uint64) string {
	return fmt.Sprintf("%schain/%016d", c.opt.Prefix, seq)
}

// Poll reads every batch appended since the handle's position, delivering
// each to Observe, and stops at the first 404.
func (c *Chain) Poll(ctx context.Context) error {
	for {
		data, _, err := c.s3.Get(ctx, c.key(c.next))
		if errors.Is(err, s3c.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		b, err := DecodeBatch(data)
		if err != nil {
			return fmt.Errorf("chain: slot %d: %w", c.next, err)
		}
		c.deliver(b)
	}
}

func (c *Chain) deliver(b *Batch) {
	if c.opt.Observe != nil {
		c.opt.Observe(c.next, b)
	}
	c.next++
}

// Append stages the records as one batch and lands it on the chain, returning
// the sequence it won. The loop per doc 04 section 8: CAS-create at the tail;
// on 412 read the winner's batch, advance, retry; on an ambiguous outcome
// re-read the slot and compare authorship (writer, incarnation, batch id) to
// learn whether the timed-out PUT actually landed. The batch bytes never
// change across retries, only the key does.
func (c *Chain) Append(ctx context.Context, records []Record) (uint64, error) {
	c.batchID++
	b := &Batch{Writer: c.opt.Writer, Incarnation: c.opt.Incarnation, BatchID: c.batchID, Records: records}
	data, err := b.Encode()
	if err != nil {
		return 0, err
	}
	for {
		seq := c.next
		_, err := c.s3.CreateExclusive(ctx, c.key(seq), data)
		if err == nil {
			c.next++
			return seq, nil
		}
		if errors.Is(err, s3c.ErrPrecondition) {
			// Lost the race: read the winner so our view advances, then
			// contend for the next slot.
			if err := c.readSlot(ctx, seq); err != nil {
				return 0, err
			}
			continue
		}
		if _, ok := errors.AsType[*s3c.AmbiguousError](err); ok {
			won, rerr := c.resolve(ctx, seq, b)
			if rerr != nil {
				return 0, rerr
			}
			if won {
				return seq, nil
			}
			continue
		}
		return 0, err
	}
}

// readSlot fetches and delivers one existing slot.
func (c *Chain) readSlot(ctx context.Context, seq uint64) error {
	data, _, err := c.s3.Get(ctx, c.key(seq))
	if err != nil {
		return fmt.Errorf("chain: read winning slot %d: %w", seq, err)
	}
	b, err := DecodeBatch(data)
	if err != nil {
		return fmt.Errorf("chain: slot %d: %w", seq, err)
	}
	c.deliver(b)
	return nil
}

// resolve settles an ambiguous append outcome. A missing slot means the PUT
// never landed and the same slot is still up for grabs; a present slot is
// ours exactly when writer, incarnation, and batch id all match, and anything
// else is a winner to fold and move past. This is the one place a write may
// be replayed, and only after the slot proved empty.
func (c *Chain) resolve(ctx context.Context, seq uint64, ours *Batch) (bool, error) {
	data, _, err := c.s3.Get(ctx, c.key(seq))
	if errors.Is(err, s3c.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chain: resolve slot %d: %w", seq, err)
	}
	b, err := DecodeBatch(data)
	if err != nil {
		return false, fmt.Errorf("chain: slot %d: %w", seq, err)
	}
	if b.Writer == ours.Writer && b.Incarnation == ours.Incarnation && b.BatchID == ours.BatchID {
		c.next++
		return true, nil
	}
	c.deliver(b)
	return false, nil
}
