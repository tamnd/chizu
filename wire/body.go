package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Body codecs for the five kinds that carry one. All fields are
// fixed-width little endian with u8/u16 length prefixes, exact
// consumption demanded, so every body has one valid byte stream and
// wire-fuzz can assert re-encode equality.

// Caps: sanity bounds a hostile peer cannot push past, all far above
// what the planner will ever send.
const (
	maxTermLen       = 64 // the doc 06 admission cap
	maxQueryTerms    = 32
	maxShards        = 1024
	maxResultEntries = 4096
	maxSnipDocs      = 1024
	maxSnipField     = 1<<16 - 1
)

// Term operators.
const (
	OpShould byte = 0
	OpMust   byte = 1
	OpNot    byte = 2
)

// Hello opens every connection: protocol version, node identity, and
// the limits the peer will enforce.
type Hello struct {
	Version     uint16
	NodeID      uint64
	MaxInflight uint32
	MaxFrame    uint32
}

func AppendHello(dst []byte, h *Hello) ([]byte, error) {
	if h.Version == 0 || h.MaxInflight == 0 || h.MaxFrame == 0 {
		return nil, errors.New("wire: hello with zero version or limits")
	}
	dst = binary.LittleEndian.AppendUint16(dst, h.Version)
	dst = binary.LittleEndian.AppendUint64(dst, h.NodeID)
	dst = binary.LittleEndian.AppendUint32(dst, h.MaxInflight)
	return binary.LittleEndian.AppendUint32(dst, h.MaxFrame), nil
}

func ParseHello(body []byte) (*Hello, error) {
	if len(body) != 18 {
		return nil, errors.New("wire: hello body is not 18 bytes")
	}
	h := &Hello{
		Version:     binary.LittleEndian.Uint16(body),
		NodeID:      binary.LittleEndian.Uint64(body[2:]),
		MaxInflight: binary.LittleEndian.Uint32(body[10:]),
		MaxFrame:    binary.LittleEndian.Uint32(body[14:]),
	}
	if h.Version == 0 || h.MaxInflight == 0 || h.MaxFrame == 0 {
		return nil, errors.New("wire: hello with zero version or limits")
	}
	return h, nil
}

// QueryTerm is one planned term with the root's df hint, which lets a
// shard order its own traversal without a dictionary round trip.
type QueryTerm struct {
	Term   []byte
	DFHint uint32
	Op     byte
}

// Query is the root-to-shard plan. BudgetUS is remaining budget in
// microseconds, never an absolute time: the receiver re-anchors it
// against its own monotonic clock at receipt, so clock skew costs only
// the one-way flight time.
type Query struct {
	BudgetUS   uint32
	Tier       byte
	Algo       byte
	K          uint16 // results wanted
	Candidates uint16 // phase-2 candidate pool
	Shards     []uint16
	Terms      []QueryTerm
}

func (q *Query) validate() error {
	if q.K == 0 || q.Candidates < q.K {
		return errors.New("wire: query candidate pool below k")
	}
	if len(q.Shards) == 0 || len(q.Shards) > maxShards {
		return fmt.Errorf("wire: query with %d shards", len(q.Shards))
	}
	if len(q.Terms) == 0 || len(q.Terms) > maxQueryTerms {
		return fmt.Errorf("wire: query with %d terms", len(q.Terms))
	}
	for _, t := range q.Terms {
		if len(t.Term) == 0 || len(t.Term) > maxTermLen {
			return fmt.Errorf("wire: query term length %d", len(t.Term))
		}
		if t.Op > OpNot {
			return fmt.Errorf("wire: unknown term operator %d", t.Op)
		}
	}
	return nil
}

func AppendQuery(dst []byte, q *Query) ([]byte, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	dst = binary.LittleEndian.AppendUint32(dst, q.BudgetUS)
	dst = append(dst, q.Tier, q.Algo)
	dst = binary.LittleEndian.AppendUint16(dst, q.K)
	dst = binary.LittleEndian.AppendUint16(dst, q.Candidates)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(q.Shards)))
	for _, s := range q.Shards {
		dst = binary.LittleEndian.AppendUint16(dst, s)
	}
	dst = append(dst, byte(len(q.Terms)))
	for _, t := range q.Terms {
		dst = append(dst, byte(len(t.Term)))
		dst = append(dst, t.Term...)
		dst = binary.LittleEndian.AppendUint32(dst, t.DFHint)
		dst = append(dst, t.Op)
	}
	return dst, nil
}

func ParseQuery(body []byte) (*Query, error) {
	if len(body) < 13 {
		return nil, errors.New("wire: query body truncated")
	}
	q := &Query{
		BudgetUS:   binary.LittleEndian.Uint32(body),
		Tier:       body[4],
		Algo:       body[5],
		K:          binary.LittleEndian.Uint16(body[6:]),
		Candidates: binary.LittleEndian.Uint16(body[8:]),
	}
	nshards := int(binary.LittleEndian.Uint16(body[10:]))
	pos := 12
	if len(body) < pos+2*nshards+1 {
		return nil, errors.New("wire: query body truncated")
	}
	q.Shards = make([]uint16, nshards)
	for i := range q.Shards {
		q.Shards[i] = binary.LittleEndian.Uint16(body[pos:])
		pos += 2
	}
	nterms := int(body[pos])
	pos++
	q.Terms = make([]QueryTerm, 0, nterms)
	for range nterms {
		if len(body) < pos+1 {
			return nil, errors.New("wire: query body truncated")
		}
		tl := int(body[pos])
		pos++
		if len(body) < pos+tl+5 {
			return nil, errors.New("wire: query body truncated")
		}
		q.Terms = append(q.Terms, QueryTerm{
			Term:   body[pos : pos+tl : pos+tl],
			DFHint: binary.LittleEndian.Uint32(body[pos+tl:]),
			Op:     body[pos+tl+4],
		})
		pos += tl + 5
	}
	if pos != len(body) {
		return nil, errors.New("wire: trailing bytes in query body")
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	return q, nil
}

// ResultEntry is one scored doc in a shard's answer: 11 bytes.
type ResultEntry struct {
	Docid uint64
	Score uint16
	Flags byte
}

// QResult is a shard's top-k answer. Degrade bits say what the shard
// gave up under its budget; Stats is the opaque per-query stats block
// the root aggregates.
type QResult struct {
	Degrade byte
	Entries []ResultEntry
	Stats   []byte
}

func AppendQResult(dst []byte, r *QResult) ([]byte, error) {
	if len(r.Entries) > maxResultEntries {
		return nil, fmt.Errorf("wire: %d result entries", len(r.Entries))
	}
	if len(r.Stats) > maxSnipField {
		return nil, errors.New("wire: stats block exceeds u16 length")
	}
	dst = append(dst, r.Degrade)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(r.Entries)))
	for _, e := range r.Entries {
		dst = binary.LittleEndian.AppendUint64(dst, e.Docid)
		dst = binary.LittleEndian.AppendUint16(dst, e.Score)
		dst = append(dst, e.Flags)
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(r.Stats)))
	return append(dst, r.Stats...), nil
}

func ParseQResult(body []byte) (*QResult, error) {
	if len(body) < 3 {
		return nil, errors.New("wire: qresult body truncated")
	}
	r := &QResult{Degrade: body[0]}
	n := int(binary.LittleEndian.Uint16(body[1:]))
	if n > maxResultEntries {
		return nil, fmt.Errorf("wire: %d result entries", n)
	}
	pos := 3
	if len(body) < pos+11*n+2 {
		return nil, errors.New("wire: qresult body truncated")
	}
	r.Entries = make([]ResultEntry, n)
	for i := range r.Entries {
		r.Entries[i] = ResultEntry{
			Docid: binary.LittleEndian.Uint64(body[pos:]),
			Score: binary.LittleEndian.Uint16(body[pos+8:]),
			Flags: body[pos+10],
		}
		pos += 11
	}
	sl := int(binary.LittleEndian.Uint16(body[pos:]))
	pos += 2
	if len(body) != pos+sl {
		return nil, errors.New("wire: qresult stats length disagrees with body")
	}
	r.Stats = body[pos : pos+sl : pos+sl]
	return r, nil
}

// Snip asks a shard to render query-biased snippets for the final
// docs. Terms ride along for biasing; the budget is what remains of
// the whole query's deadline.
type Snip struct {
	BudgetUS uint32
	Terms    [][]byte
	Docids   []uint64
}

func (s *Snip) validate() error {
	if len(s.Terms) == 0 || len(s.Terms) > maxQueryTerms {
		return fmt.Errorf("wire: snip with %d terms", len(s.Terms))
	}
	for _, t := range s.Terms {
		if len(t) == 0 || len(t) > maxTermLen {
			return fmt.Errorf("wire: snip term length %d", len(t))
		}
	}
	if len(s.Docids) == 0 || len(s.Docids) > maxSnipDocs {
		return fmt.Errorf("wire: snip with %d docids", len(s.Docids))
	}
	return nil
}

func AppendSnip(dst []byte, s *Snip) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	dst = binary.LittleEndian.AppendUint32(dst, s.BudgetUS)
	dst = append(dst, byte(len(s.Terms)))
	for _, t := range s.Terms {
		dst = append(dst, byte(len(t)))
		dst = append(dst, t...)
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(s.Docids)))
	for _, d := range s.Docids {
		dst = binary.LittleEndian.AppendUint64(dst, d)
	}
	return dst, nil
}

func ParseSnip(body []byte) (*Snip, error) {
	if len(body) < 5 {
		return nil, errors.New("wire: snip body truncated")
	}
	s := &Snip{BudgetUS: binary.LittleEndian.Uint32(body)}
	nterms := int(body[4])
	pos := 5
	s.Terms = make([][]byte, 0, nterms)
	for range nterms {
		if len(body) < pos+1 {
			return nil, errors.New("wire: snip body truncated")
		}
		tl := int(body[pos])
		pos++
		if len(body) < pos+tl {
			return nil, errors.New("wire: snip body truncated")
		}
		s.Terms = append(s.Terms, body[pos:pos+tl:pos+tl])
		pos += tl
	}
	if len(body) < pos+2 {
		return nil, errors.New("wire: snip body truncated")
	}
	n := int(binary.LittleEndian.Uint16(body[pos:]))
	pos += 2
	if len(body) != pos+8*n {
		return nil, errors.New("wire: snip docid array disagrees with body")
	}
	s.Docids = make([]uint64, n)
	for i := range s.Docids {
		s.Docids[i] = binary.LittleEndian.Uint64(body[pos:])
		pos += 8
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// SnippetRecord is one rendered snippet.
type SnippetRecord struct {
	Docid   uint64
	URL     []byte
	Title   []byte
	Snippet []byte
}

// SnipResult carries the rendered records back.
type SnipResult struct {
	Records []SnippetRecord
}

func AppendSnipResult(dst []byte, r *SnipResult) ([]byte, error) {
	if len(r.Records) == 0 || len(r.Records) > maxSnipDocs {
		return nil, fmt.Errorf("wire: snipresult with %d records", len(r.Records))
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(r.Records)))
	for _, rec := range r.Records {
		if len(rec.URL) == 0 {
			return nil, errors.New("wire: snippet record with empty url")
		}
		if len(rec.URL) > maxSnipField || len(rec.Title) > maxSnipField || len(rec.Snippet) > maxSnipField {
			return nil, errors.New("wire: snippet record field exceeds u16 length")
		}
		dst = binary.LittleEndian.AppendUint64(dst, rec.Docid)
		for _, f := range [][]byte{rec.URL, rec.Title, rec.Snippet} {
			dst = binary.LittleEndian.AppendUint16(dst, uint16(len(f)))
			dst = append(dst, f...)
		}
	}
	return dst, nil
}

func ParseSnipResult(body []byte) (*SnipResult, error) {
	if len(body) < 2 {
		return nil, errors.New("wire: snipresult body truncated")
	}
	n := int(binary.LittleEndian.Uint16(body))
	if n == 0 || n > maxSnipDocs {
		return nil, fmt.Errorf("wire: snipresult with %d records", n)
	}
	r := &SnipResult{Records: make([]SnippetRecord, 0, n)}
	pos := 2
	for range n {
		if len(body) < pos+8 {
			return nil, errors.New("wire: snipresult body truncated")
		}
		rec := SnippetRecord{Docid: binary.LittleEndian.Uint64(body[pos:])}
		pos += 8
		for _, f := range []*[]byte{&rec.URL, &rec.Title, &rec.Snippet} {
			if len(body) < pos+2 {
				return nil, errors.New("wire: snipresult body truncated")
			}
			fl := int(binary.LittleEndian.Uint16(body[pos:]))
			pos += 2
			if len(body) < pos+fl {
				return nil, errors.New("wire: snipresult body truncated")
			}
			*f = body[pos : pos+fl : pos+fl]
			pos += fl
		}
		if len(rec.URL) == 0 {
			return nil, errors.New("wire: snippet record with empty url")
		}
		r.Records = append(r.Records, rec)
	}
	if pos != len(body) {
		return nil, errors.New("wire: trailing bytes in snipresult body")
	}
	return r, nil
}
