package chain

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/tamnd/chizu/s3c"
)

// CkptInterval is the checkpoint cadence: the appender that lands the slot
// completing each interval folds chain state into chain/ckpt/<seq16>
// (doc 02 section 4). A knob, not a format constant; tests shrink it.
const CkptInterval = 4096

func ckptKey(prefix string, seq uint64) string {
	return fmt.Sprintf("%schain/ckpt/%016d", prefix, seq)
}

// EncodeCheckpoint lays out format 0x7A03: common header, this checkpoint's
// slot, the CkptSeq the fold had reached, then the five sections of doc 04
// section 8 (members, leases, manifest pointers, generations, tier map),
// each length-prefixed and crc'd, and the trailing body crc.
//
// Determinism is the point: sections iterate in sorted key order, and the
// header's writer field is the writer of the batch that landed the
// triggering slot, itself a fact of the chain. Racing checkpoint writers
// therefore produce byte-identical objects, and the CAS-create below makes
// whichever lands first the only one.
func (s *State) EncodeCheckpoint(writer, seq uint64) []byte {
	out := appendHeader(make([]byte, 0, 1024), formatCkpt, writer)
	out = binary.LittleEndian.AppendUint64(out, seq)
	out = binary.LittleEndian.AppendUint64(out, s.CkptSeq)

	out = section(out, func(dst []byte) []byte {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s.Members)))
		for _, node := range slices.Sorted(maps.Keys(s.Members)) {
			m := s.Members[node]
			dst = binary.LittleEndian.AppendUint64(dst, m.Node)
			dst = append(dst, m.Plane)
			dst = binary.LittleEndian.AppendUint32(dst, m.Incarnation)
			dst = append(dst, byte(len(m.Endpoints)))
			for _, e := range m.Endpoints {
				dst = appendStr(dst, e)
			}
			dst = appendStr(dst, m.Version)
		}
		return dst
	})
	out = section(out, func(dst []byte) []byte {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s.Leases.m)))
		for _, k := range slices.SortedFunc(maps.Keys(s.Leases.m), cmpLease) {
			l := s.Leases.m[k]
			dst = append(dst, l.Domain)
			dst = binary.LittleEndian.AppendUint32(dst, l.Unit)
			dst = binary.LittleEndian.AppendUint64(dst, l.Node)
			dst = binary.LittleEndian.AppendUint32(dst, l.Epoch)
			dst = binary.LittleEndian.AppendUint64(dst, l.DeadlineMS)
			if l.Released {
				dst = append(dst, 1)
			} else {
				dst = append(dst, 0)
			}
		}
		return dst
	})
	out = section(out, func(dst []byte) []byte {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s.Mans)))
		for _, k := range slices.SortedFunc(maps.Keys(s.Mans), cmpMan) {
			dst = append(dst, k.Family)
			dst = binary.LittleEndian.AppendUint16(dst, k.Partition)
			dst = binary.LittleEndian.AppendUint64(dst, s.Mans[k])
		}
		return dst
	})
	out = section(out, func(dst []byte) []byte {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s.Gens)))
		for _, k := range slices.SortedFunc(maps.Keys(s.Gens), cmpGen) {
			dst = binary.LittleEndian.AppendUint16(dst, k.Shard)
			dst = binary.LittleEndian.AppendUint64(dst, k.Generation)
			dst = append(dst, s.Gens[k])
		}
		return dst
	})
	out = section(out, func(dst []byte) []byte {
		dst = binary.LittleEndian.AppendUint64(dst, s.TierVersion)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s.Tiers)))
		for _, shard := range slices.Sorted(maps.Keys(s.Tiers)) {
			dst = binary.LittleEndian.AppendUint16(dst, shard)
			dst = append(dst, s.Tiers[shard])
		}
		return dst
	})

	return binary.LittleEndian.AppendUint32(out, crc(out[headerSize:]))
}

// section wraps one encoded section in its u32 length prefix and u32 crc.
func section(out []byte, encode func([]byte) []byte) []byte {
	at := len(out)
	out = append(out, 0, 0, 0, 0)
	out = encode(out)
	body := out[at+4:]
	binary.LittleEndian.PutUint32(out[at:], uint32(len(body)))
	return binary.LittleEndian.AppendUint32(out, crc(body))
}

func cmpLease(a, b LeaseKey) int {
	if c := cmp.Compare(a.Domain, b.Domain); c != 0 {
		return c
	}
	return cmp.Compare(a.Unit, b.Unit)
}

func cmpMan(a, b ManKey) int {
	if c := cmp.Compare(a.Family, b.Family); c != 0 {
		return c
	}
	return cmp.Compare(a.Partition, b.Partition)
}

func cmpGen(a, b GenKey) int {
	if c := cmp.Compare(a.Shard, b.Shard); c != 0 {
		return c
	}
	return cmp.Compare(a.Generation, b.Generation)
}

// DecodeCheckpoint parses and fully verifies one checkpoint object,
// returning the reconstructed fold state, the triggering slot, and the
// writer that landed it.
func DecodeCheckpoint(data []byte) (*State, uint64, uint64, error) {
	writer, payload, err := openObject(data, formatCkpt)
	if err != nil {
		return nil, 0, 0, err
	}
	r := &reader{b: payload}
	seq := r.u64()
	s := NewState()
	s.CkptSeq = r.u64()

	sections := make([]*reader, 5)
	for i := range sections {
		n := int(r.u32())
		body := r.take(n)
		sum := r.u32()
		if r.err != nil {
			return nil, 0, 0, fmt.Errorf("chain: checkpoint section %d: %w", i, r.err)
		}
		if sum != crc(body) {
			return nil, 0, 0, fmt.Errorf("chain: checkpoint section %d crc mismatch", i)
		}
		sections[i] = &reader{b: body}
	}
	if len(r.b) != 0 {
		return nil, 0, 0, fmt.Errorf("chain: %d trailing bytes after checkpoint sections", len(r.b))
	}

	mr := sections[0]
	for range int(mr.u32()) {
		m := Member{Node: mr.u64(), Plane: mr.u8(), Incarnation: mr.u32()}
		for range int(mr.u8()) {
			m.Endpoints = append(m.Endpoints, mr.str())
		}
		m.Version = mr.str()
		s.Members[m.Node] = m
	}
	lr := sections[1]
	for range int(lr.u32()) {
		l := Lease{Domain: lr.u8(), Unit: lr.u32(), Node: lr.u64(), Epoch: lr.u32(), DeadlineMS: lr.u64(), Released: lr.u8() == 1}
		s.Leases.m[LeaseKey{l.Domain, l.Unit}] = l
	}
	nr := sections[2]
	for range int(nr.u32()) {
		k := ManKey{Family: nr.u8(), Partition: nr.u16()}
		s.Mans[k] = nr.u64()
	}
	gr := sections[3]
	for range int(gr.u32()) {
		k := GenKey{Shard: gr.u16(), Generation: gr.u64()}
		s.Gens[k] = gr.u8()
	}
	tr := sections[4]
	s.TierVersion = tr.u64()
	for range int(tr.u32()) {
		shard := tr.u16()
		s.Tiers[shard] = tr.u8()
	}
	for i, sec := range sections {
		if sec.err != nil {
			return nil, 0, 0, fmt.Errorf("chain: checkpoint section %d: %w", i, sec.err)
		}
		if len(sec.b) != 0 {
			return nil, 0, 0, fmt.Errorf("chain: %d trailing bytes in checkpoint section %d", len(sec.b), i)
		}
	}
	return s, seq, writer, nil
}

// Checkpointer folds the chain through a State and captures checkpoint
// bytes exactly at each interval boundary. Wire its Observe into a chain
// handle; after Append or Poll returns, Flush writes whatever boundaries
// were crossed. Not safe for concurrent use, same as the handle it feeds.
type Checkpointer struct {
	State    *State
	Interval uint64
	pending  []pendingCkpt
}

type pendingCkpt struct {
	seq  uint64
	data []byte
}

// NewCheckpointer starts an empty fold at the default interval. To boot from
// a checkpoint, replace State with the decoded one before observing the tail.
func NewCheckpointer() *Checkpointer {
	return &Checkpointer{State: NewState(), Interval: CkptInterval}
}

// Observe folds one batch and, on a triggering slot, snapshots the
// checkpoint bytes while the State is exactly at the boundary.
func (c *Checkpointer) Observe(seq uint64, b *Batch) {
	c.State.Apply(b)
	if (seq+1)%c.Interval == 0 {
		c.pending = append(c.pending, pendingCkpt{seq: seq, data: c.State.EncodeCheckpoint(b.Writer, seq)})
	}
}

// Flush CAS-creates every pending checkpoint and returns the slots that are
// now durably checkpointed. Losing the create race is success: the winner
// wrote byte-identical content. The caller should append a Ckpt record for
// each returned slot; duplicates fold idempotently, so racing announcers
// are harmless too.
func (c *Checkpointer) Flush(ctx context.Context, client *s3c.Client, prefix string) ([]uint64, error) {
	var done []uint64
	for len(c.pending) > 0 {
		p := c.pending[0]
		_, err := client.CreateExclusive(ctx, ckptKey(prefix, p.seq), p.data)
		if err != nil && !errors.Is(err, s3c.ErrPrecondition) {
			return done, err
		}
		done = append(done, p.seq)
		c.pending = c.pending[1:]
	}
	return done, nil
}

// TrimBehind deletes the chain slots at least two checkpoints old: the
// interval ending at the second-newest checkpoint (doc 02 section 4). One
// checkpoint of slack stays readable behind the newest, so a node booting
// from a root that has not caught up to the newest checkpoint still finds
// its tail. Callers after a fresh checkpoint pass the slot Flush returned;
// each call removes exactly one interval, which is also why a missed trim
// self-heals on the next one only if run for the skipped boundary too.
//
// After a trim, slot 0 is gone: discovery must go through the root's
// checkpoint pointer, never a bare probe from zero.
func TrimBehind(ctx context.Context, client *s3c.Client, prefix string, newestCkpt, interval uint64) error {
	if newestCkpt+1 < 2*interval {
		return nil
	}
	second := newestCkpt - interval
	lo := second + 1 - interval
	keys := make([]string, 0, interval)
	for seq := lo; seq <= second; seq++ {
		keys = append(keys, fmt.Sprintf("%schain/%016d", prefix, seq))
	}
	return client.DeleteBatch(ctx, keys)
}
