package chain

// State is the full folded chain state: everything a checkpoint carries
// (doc 04 section 8) and everything a booting node needs before it can act.
// The fold is a pure function of the records in chain order, so every node
// at the same position holds the identical State (doc 02 A-I5).
type State struct {
	// Members holds the newest member record per node; membership and
	// heartbeat ride the same record kind.
	Members map[uint64]Member
	// Leases is the doc 02 section 5 lease fold.
	Leases *LeaseTable
	// Mans maps (family, partition) to the newest manifest sequence. The
	// pointer only moves forward; whether a manifest's content is valid is
	// the reader-side epoch rule of doc 04 section 9, not the fold's call.
	Mans map[ManKey]uint64
	// Gens is the live generation set: (shard, generation) to kind.
	Gens map[GenKey]byte
	// Tiers is the newest tier map, flattened; TierVersion arbitrates.
	Tiers       map[uint16]byte
	TierVersion uint64
	// CkptSeq is the newest checkpoint the chain itself has announced.
	CkptSeq uint64
}

// ManKey names one manifest family instance.
type ManKey struct {
	Family    byte
	Partition uint16
}

// GenKey names one published generation.
type GenKey struct {
	Shard      uint16
	Generation uint64
}

func NewState() *State {
	return &State{
		Members: map[uint64]Member{},
		Leases:  NewLeaseTable(),
		Mans:    map[ManKey]uint64{},
		Gens:    map[GenKey]byte{},
		Tiers:   map[uint16]byte{},
	}
}

// Apply folds one batch, in record order. Seg-commits pass through: the
// segment ledger lives in manifests, and boot replays the chain tail for
// anything a manifest has not caught up to yet.
func (s *State) Apply(b *Batch) {
	for _, r := range b.Records {
		switch v := r.(type) {
		case *Member:
			s.Members[v.Node] = *v
		case *LeaseGrant, *LeaseRelease:
			s.Leases.applyRecord(r)
		case *ManAdvance:
			if k := (ManKey{v.Family, v.Partition}); v.ManSeq > s.Mans[k] {
				s.Mans[k] = v.ManSeq
			}
		case *GenPublish:
			s.Gens[GenKey{v.Shard, v.Generation}] = v.GenKind
		case *GenRetire:
			delete(s.Gens, GenKey{v.Shard, v.Generation})
		case *TierMap:
			if v.Version > s.TierVersion {
				s.TierVersion = v.Version
				clear(s.Tiers)
				for _, a := range v.Assign {
					s.Tiers[a.Shard] = a.Tier
				}
			}
		case *Ckpt:
			if v.Seq > s.CkptSeq {
				s.CkptSeq = v.Seq
			}
		}
	}
}
