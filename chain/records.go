package chain

import (
	"encoding/binary"
	"fmt"
)

// Record is one entry in a batch. The nine kinds are the doc 02 section 4
// table; bodies are flat little-endian structs, and every epoch-scoped kind
// (lease and commit kinds) begins with the writer's epoch.
type Record interface {
	Kind() byte
	encodeBody(dst []byte) []byte
}

// Record kinds, in doc 02 section 4 table order.
const (
	KindMember       byte = 1
	KindLeaseGrant   byte = 2
	KindLeaseRelease byte = 3
	KindSegCommit    byte = 4
	KindManAdvance   byte = 5
	KindGenPublish   byte = 6
	KindGenRetire    byte = 7
	KindTierMap      byte = 8
	KindCkpt         byte = 9
)

// Planes for Member records.
const (
	PlaneCrawl byte = 1
	PlaneBuild byte = 2
	PlaneServe byte = 3
	PlaneRoot  byte = 4
)

// Lease domains (doc 02 section 5).
const (
	DomainCrawlPart byte = 1
	DomainBuildJob  byte = 2
	DomainMergeJob  byte = 3
)

// .cold families named by commit and manifest records (doc 04).
const (
	FamilyPage     byte = 1
	FamilyRaw      byte = 2
	FamilyFrontier byte = 3
	FamilyGraph    byte = 4
	FamilyDocmap   byte = 5
)

// Generation kinds for GenPublish.
const (
	GenBase  byte = 1
	GenDelta byte = 2
)

// Member is the join/leave/heartbeat carrier.
type Member struct {
	Node        uint64
	Plane       byte
	Incarnation uint32
	Endpoints   []string
	Version     string
}

func (*Member) Kind() byte { return KindMember }

func (m *Member) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, m.Node)
	dst = append(dst, m.Plane)
	dst = binary.LittleEndian.AppendUint32(dst, m.Incarnation)
	dst = append(dst, byte(len(m.Endpoints)))
	for _, e := range m.Endpoints {
		dst = appendStr(dst, e)
	}
	return appendStr(dst, m.Version)
}

// LeaseGrant gives one node one unit until the deadline; epoch increments on
// every grant of the same unit.
type LeaseGrant struct {
	Epoch      uint32
	Domain     byte
	Unit       uint32
	Node       uint64
	DeadlineMS uint64
}

func (*LeaseGrant) Kind() byte { return KindLeaseGrant }

func (g *LeaseGrant) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, g.Epoch)
	dst = append(dst, g.Domain)
	dst = binary.LittleEndian.AppendUint32(dst, g.Unit)
	dst = binary.LittleEndian.AppendUint64(dst, g.Node)
	return binary.LittleEndian.AppendUint64(dst, g.DeadlineMS)
}

// LeaseRelease is the cooperative give-back.
type LeaseRelease struct {
	Epoch  uint32
	Domain byte
	Unit   uint32
}

func (*LeaseRelease) Kind() byte { return KindLeaseRelease }

func (l *LeaseRelease) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, l.Epoch)
	dst = append(dst, l.Domain)
	return binary.LittleEndian.AppendUint32(dst, l.Unit)
}

// SegCommit makes a .cold segment live; an uploaded object without this
// record does not exist (doc 02 section 5).
type SegCommit struct {
	Epoch     uint32
	Family    byte
	Partition uint16
	Seq       uint64
	Rows      uint64
	Bytes     uint64
	Watermark uint64
}

func (*SegCommit) Kind() byte { return KindSegCommit }

func (s *SegCommit) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, s.Epoch)
	dst = append(dst, s.Family)
	dst = binary.LittleEndian.AppendUint16(dst, s.Partition)
	dst = binary.LittleEndian.AppendUint64(dst, s.Seq)
	dst = binary.LittleEndian.AppendUint64(dst, s.Rows)
	dst = binary.LittleEndian.AppendUint64(dst, s.Bytes)
	return binary.LittleEndian.AppendUint64(dst, s.Watermark)
}

// ManAdvance records that a manifest superseded its predecessors.
type ManAdvance struct {
	Epoch     uint32
	Family    byte
	Partition uint16
	ManSeq    uint64
}

func (*ManAdvance) Kind() byte { return KindManAdvance }

func (m *ManAdvance) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, m.Epoch)
	dst = append(dst, m.Family)
	dst = binary.LittleEndian.AppendUint16(dst, m.Partition)
	return binary.LittleEndian.AppendUint64(dst, m.ManSeq)
}

// SourceWatermark names how far into one crawl partition a generation's
// input reached.
type SourceWatermark struct {
	Partition uint16
	Seq       uint64
}

// GenPublish makes a .hot generation servable.
type GenPublish struct {
	Epoch      uint32
	Shard      uint16
	Generation uint64
	GenKind    byte
	Watermarks []SourceWatermark
}

func (*GenPublish) Kind() byte { return KindGenPublish }

func (g *GenPublish) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, g.Epoch)
	dst = binary.LittleEndian.AppendUint16(dst, g.Shard)
	dst = binary.LittleEndian.AppendUint64(dst, g.Generation)
	dst = append(dst, g.GenKind)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(g.Watermarks)))
	for _, w := range g.Watermarks {
		dst = binary.LittleEndian.AppendUint16(dst, w.Partition)
		dst = binary.LittleEndian.AppendUint64(dst, w.Seq)
	}
	return dst
}

// GenRetire removes a generation from the servable set.
type GenRetire struct {
	Shard      uint16
	Generation uint64
}

func (*GenRetire) Kind() byte { return KindGenRetire }

func (g *GenRetire) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, g.Shard)
	return binary.LittleEndian.AppendUint64(dst, g.Generation)
}

// TierAssign maps one shard to a serving tier.
type TierAssign struct {
	Shard uint16
	Tier  byte
}

// TierMap is a prime/full tier membership change.
type TierMap struct {
	Version uint64
	Assign  []TierAssign
}

func (*TierMap) Kind() byte { return KindTierMap }

func (t *TierMap) encodeBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, t.Version)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(t.Assign)))
	for _, a := range t.Assign {
		dst = binary.LittleEndian.AppendUint16(dst, a.Shard)
		dst = append(dst, a.Tier)
	}
	return dst
}

// Ckpt points at a chain checkpoint object.
type Ckpt struct {
	Seq uint64
}

func (*Ckpt) Kind() byte { return KindCkpt }

func (c *Ckpt) encodeBody(dst []byte) []byte {
	return binary.LittleEndian.AppendUint64(dst, c.Seq)
}

// decodeRecord parses one body and demands full consumption; an unknown kind
// is an error, because a reader that cannot fold a record cannot claim to
// know the chain state.
func decodeRecord(kind byte, body []byte) (Record, error) {
	r := &reader{b: body}
	var rec Record
	switch kind {
	case KindMember:
		m := &Member{Node: r.u64(), Plane: r.u8(), Incarnation: r.u32()}
		n := int(r.u8())
		for range n {
			m.Endpoints = append(m.Endpoints, r.str())
		}
		m.Version = r.str()
		rec = m
	case KindLeaseGrant:
		rec = &LeaseGrant{Epoch: r.u32(), Domain: r.u8(), Unit: r.u32(), Node: r.u64(), DeadlineMS: r.u64()}
	case KindLeaseRelease:
		rec = &LeaseRelease{Epoch: r.u32(), Domain: r.u8(), Unit: r.u32()}
	case KindSegCommit:
		rec = &SegCommit{Epoch: r.u32(), Family: r.u8(), Partition: r.u16(), Seq: r.u64(), Rows: r.u64(), Bytes: r.u64(), Watermark: r.u64()}
	case KindManAdvance:
		rec = &ManAdvance{Epoch: r.u32(), Family: r.u8(), Partition: r.u16(), ManSeq: r.u64()}
	case KindGenPublish:
		g := &GenPublish{Epoch: r.u32(), Shard: r.u16(), Generation: r.u64(), GenKind: r.u8()}
		n := int(r.u16())
		for range n {
			g.Watermarks = append(g.Watermarks, SourceWatermark{Partition: r.u16(), Seq: r.u64()})
		}
		rec = g
	case KindGenRetire:
		rec = &GenRetire{Shard: r.u16(), Generation: r.u64()}
	case KindTierMap:
		t := &TierMap{Version: r.u64()}
		n := int(r.u16())
		for range n {
			t.Assign = append(t.Assign, TierAssign{Shard: r.u16(), Tier: r.u8()})
		}
		rec = t
	case KindCkpt:
		rec = &Ckpt{Seq: r.u64()}
	default:
		return nil, fmt.Errorf("chain: unknown record kind %d", kind)
	}
	if r.err != nil {
		return nil, fmt.Errorf("chain: kind-%d record: %w", kind, r.err)
	}
	if len(r.b) != 0 {
		return nil, fmt.Errorf("chain: %d trailing bytes in kind-%d record", len(r.b), kind)
	}
	return rec, nil
}
