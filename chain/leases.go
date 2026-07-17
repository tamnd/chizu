package chain

// Leases make ownership exclusive in time; epochs make stale owners harmless
// (doc 02 section 5). The fold below is pure record application in chain
// order, so every reader computes the identical table: the chain arbitrates
// races, holder and taker discipline live with the appenders, and fencing is
// the epoch comparison at commit time.

// Lease TTLs by domain, doc 02 section 5 defaults.
const (
	TTLCrawlMS = 60_000
	TTLBuildMS = 900_000
	TTLMergeMS = 900_000
)

// SuspendSlackMS is the renewal slack: a holder inside this window of its
// deadline without a confirmed renewal stops writing. The slack absorbs
// append latency jitter, so a disciplined holder is never writing while
// another node could legally hold the unit.
const SuspendSlackMS = 5_000

// DefaultTTLMS returns the grant TTL for a domain.
func DefaultTTLMS(domain byte) uint64 {
	if domain == DomainCrawlPart {
		return TTLCrawlMS
	}
	return TTLBuildMS
}

// LeaseKey names one leasable unit.
type LeaseKey struct {
	Domain byte
	Unit   uint32
}

// Lease is the folded state of one unit.
type Lease struct {
	Domain     byte
	Unit       uint32
	Node       uint64
	Epoch      uint32
	DeadlineMS uint64
	Released   bool
}

// Expired reports whether the unit is up for grabs at nowMS. The epoch it
// held stays burned either way.
func (l Lease) Expired(nowMS uint64) bool {
	return l.Released || nowMS > l.DeadlineMS
}

// RenewalDue reports whether the holder has passed the half-life of its TTL
// and should carry a renewal grant in its next append.
func (l Lease) RenewalDue(nowMS uint64) bool {
	return nowMS+DefaultTTLMS(l.Domain)/2 >= l.DeadlineMS
}

// MustSuspend is holder discipline: within one renewal slack of the deadline
// with no confirmed renewal, all writes for the unit stop.
func (l Lease) MustSuspend(nowMS uint64) bool {
	return nowMS+SuspendSlackMS >= l.DeadlineMS
}

// LeaseTable folds lease records into per-unit state. Not safe for
// concurrent use; each node folds on its own poll loop.
type LeaseTable struct {
	m map[LeaseKey]Lease
}

func NewLeaseTable() *LeaseTable {
	return &LeaseTable{m: map[LeaseKey]Lease{}}
}

// Apply folds every lease record of one batch, in order. Other record kinds
// pass through untouched.
func (t *LeaseTable) Apply(b *Batch) {
	for _, r := range b.Records {
		t.applyRecord(r)
	}
}

func (t *LeaseTable) applyRecord(r Record) {
	switch g := r.(type) {
	case *LeaseGrant:
		k := LeaseKey{g.Domain, g.Unit}
		cur, ok := t.m[k]
		switch {
		case !ok, g.Epoch > cur.Epoch:
			// First grant, or a take. The fold accepts any forward epoch:
			// chain order already arbitrated the race, and the taker's duty
			// (claim only after observing the chain past the deadline) was
			// discharged before the append.
			t.m[k] = Lease{Domain: g.Domain, Unit: g.Unit, Node: g.Node, Epoch: g.Epoch, DeadlineMS: g.DeadlineMS}
		case g.Epoch == cur.Epoch && g.Node == cur.Node && !cur.Released:
			// Renewal by the sitting holder: the deadline only moves forward.
			if g.DeadlineMS > cur.DeadlineMS {
				cur.DeadlineMS = g.DeadlineMS
				t.m[k] = cur
			}
		default:
			// Stale epoch or a stranger reusing the current one: a zombie's
			// append, ignored.
		}
	case *LeaseRelease:
		k := LeaseKey{g.Domain, g.Unit}
		if cur, ok := t.m[k]; ok && g.Epoch == cur.Epoch && !cur.Released {
			cur.Released = true
			t.m[k] = cur
		}
	}
}

// Get returns the folded lease for a unit.
func (t *LeaseTable) Get(domain byte, unit uint32) (Lease, bool) {
	l, ok := t.m[LeaseKey{domain, unit}]
	return l, ok
}

// Claimable is taker discipline: it answers whether a unit may be claimed at
// nowMS and with which epoch. The caller must be at the chain tail when it
// asks; observing the chain past the deadline slot is what makes a stale
// holder's slack arithmetic sound.
func (t *LeaseTable) Claimable(domain byte, unit uint32, nowMS uint64) (uint32, bool) {
	cur, ok := t.m[LeaseKey{domain, unit}]
	if !ok {
		return 1, true
	}
	if cur.Expired(nowMS) {
		return cur.Epoch + 1, true
	}
	return 0, false
}

// HoldsEpoch is the fence: it reports whether node currently holds the unit
// at exactly this epoch. A seg-commit (or any epoch-carrying write) folded
// when this is false comes from a zombie and is ignored, per the doc 02
// epoch-validity rule.
func (t *LeaseTable) HoldsEpoch(domain byte, unit uint32, node uint64, epoch uint32) bool {
	cur, ok := t.m[LeaseKey{domain, unit}]
	return ok && !cur.Released && cur.Node == node && cur.Epoch == epoch
}
