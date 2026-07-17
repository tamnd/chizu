package chain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tamnd/chizu/s3c"
)

// Root is the one mutable object in the system (format 0x7A01, doc 04
// section 8): identity and frozen policy written once at create, plus the
// checkpoint pointer that advances by CAS-replace.
type Root struct {
	Writer    uint64
	DBID      uint64
	CreatedMS uint64
	// P is the crawl partition count and ShardSize the docs-per-shard
	// policy, both frozen at create.
	P         uint16
	ShardSize uint32
	// Frozen carries law version, tokenizer version, quantization policy,
	// and field schema; opaque here, typed by the format packages that own
	// those layouts.
	Frozen  []byte
	CkptSeq uint64
}

// Encode lays the root out per doc 04 section 8.
func (r *Root) Encode() ([]byte, error) {
	out := appendHeader(make([]byte, 0, 96+len(r.Frozen)), formatRoot, r.Writer)
	out = binary.LittleEndian.AppendUint64(out, r.DBID)
	out = binary.LittleEndian.AppendUint64(out, r.CreatedMS)
	out = binary.LittleEndian.AppendUint16(out, r.P)
	out = binary.LittleEndian.AppendUint32(out, r.ShardSize)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(r.Frozen)))
	out = append(out, r.Frozen...)
	out = binary.LittleEndian.AppendUint64(out, r.CkptSeq)
	out = binary.LittleEndian.AppendUint32(out, crc(out[headerSize:]))
	return out, nil
}

// DecodeRoot parses and fully verifies one root object.
func DecodeRoot(data []byte) (*Root, error) {
	writer, payload, err := openObject(data, formatRoot)
	if err != nil {
		return nil, err
	}
	rd := &reader{b: payload}
	r := &Root{
		Writer:    writer,
		DBID:      rd.u64(),
		CreatedMS: rd.u64(),
		P:         rd.u16(),
		ShardSize: rd.u32(),
	}
	n := int(rd.u32())
	if blob := rd.take(n); len(blob) > 0 {
		r.Frozen = append([]byte(nil), blob...)
	}
	r.CkptSeq = rd.u64()
	if rd.err != nil {
		return nil, fmt.Errorf("chain: root: %w", rd.err)
	}
	if len(rd.b) != 0 {
		return nil, fmt.Errorf("chain: %d trailing bytes after root", len(rd.b))
	}
	return r, nil
}

// RootStore reads and advances the root through one of the two doc 04
// discovery paths: the single CAS-replaced `root` object, or, on providers
// without If-Match, the dense `rootv/<seq16>` sequence where the newest
// object is the truth and advance is a CAS-create of the next slot.
type RootStore struct {
	s3     *s3c.Client
	prefix string
	writer uint64
	seq    bool
	etag   string // cas mode: etag of the last root read or written
	next   uint64 // seq mode: first unprobed rootv sequence
	cur    *Root  // seq mode: newest root seen so far
}

// NewRootStore returns a store for the caller-chosen database prefix.
// seqFallback selects the rootv/ path. Like Chain, a store is a
// single-goroutine handle.
func NewRootStore(client *s3c.Client, prefix string, writer uint64, seqFallback bool) *RootStore {
	return &RootStore{s3: client, prefix: prefix, writer: writer, seq: seqFallback}
}

func (s *RootStore) key() string { return s.prefix + "root" }

func (s *RootStore) seqKey(n uint64) string {
	return fmt.Sprintf("%srootv/%016d", s.prefix, n)
}

// Create writes the initial root. ErrPrecondition means the database already
// exists.
func (s *RootStore) Create(ctx context.Context, r *Root) error {
	rr := *r
	rr.Writer = s.writer
	data, err := rr.Encode()
	if err != nil {
		return err
	}
	if s.seq {
		if _, err := s.s3.CreateExclusive(ctx, s.seqKey(0), data); err != nil {
			return err
		}
		s.cur, s.next = &rr, 1
		return nil
	}
	etag, err := s.s3.CreateExclusive(ctx, s.key(), data)
	if err != nil {
		return err
	}
	s.etag = etag
	return nil
}

// Load returns the current root: a fresh GET in cas mode, GET-next-until-404
// from the last known position in seq mode. s3c.ErrNotFound means no
// database lives under this prefix.
func (s *RootStore) Load(ctx context.Context) (*Root, error) {
	if !s.seq {
		data, etag, err := s.s3.Get(ctx, s.key())
		if err != nil {
			return nil, err
		}
		r, err := DecodeRoot(data)
		if err != nil {
			return nil, err
		}
		s.etag = etag
		return r, nil
	}
	for {
		data, _, err := s.s3.Get(ctx, s.seqKey(s.next))
		if errors.Is(err, s3c.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		r, err := DecodeRoot(data)
		if err != nil {
			return nil, fmt.Errorf("chain: rootv %d: %w", s.next, err)
		}
		s.cur = r
		s.next++
	}
	if s.cur == nil {
		return nil, s3c.ErrNotFound
	}
	return s.cur, nil
}

// Advance moves the checkpoint pointer forward to ckptSeq and returns the
// root that ended up current. Forward-only: if a racer already advanced to
// ckptSeq or past it, that root is the answer and nothing is written. That
// monotonicity is also what makes retry safe after an ambiguous outcome: the
// loop re-reads, and a replayed write that already landed just loses its CAS.
func (s *RootStore) Advance(ctx context.Context, ckptSeq uint64) (*Root, error) {
	for {
		cur, err := s.Load(ctx)
		if err != nil {
			return nil, err
		}
		if cur.CkptSeq >= ckptSeq {
			return cur, nil
		}
		nw := *cur
		nw.CkptSeq = ckptSeq
		nw.Writer = s.writer
		data, err := nw.Encode()
		if err != nil {
			return nil, err
		}
		if s.seq {
			_, err = s.s3.CreateExclusive(ctx, s.seqKey(s.next), data)
			if err == nil {
				s.cur = &nw
				s.next++
				return &nw, nil
			}
		} else {
			var etag string
			etag, err = s.s3.ReplaceIfMatch(ctx, s.key(), data, s.etag)
			if err == nil {
				s.etag = etag
				return &nw, nil
			}
		}
		if errors.Is(err, s3c.ErrPrecondition) {
			continue
		}
		if _, ok := errors.AsType[*s3c.AmbiguousError](err); ok {
			continue
		}
		return nil, err
	}
}
