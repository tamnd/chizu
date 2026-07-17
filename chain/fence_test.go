package chain

import (
	"context"
	"math/rand"
	"testing"
)

// acceptedCommit is one seg-commit that survived the fence during replay.
type acceptedCommit struct {
	unit   uint32
	epoch  uint32
	writer uint64
	seq    uint64
}

// replayWithFence folds the whole chain from zero and applies the doc 02
// epoch-validity rule to every seg-commit: accepted only if its batch's
// writer holds the unit at exactly the record's epoch when the record folds.
func replayWithFence(t *testing.T, c *Chain) (accepted, rejected []acceptedCommit) {
	t.Helper()
	tab := NewLeaseTable()
	fold := func(seq uint64, b *Batch) {
		for _, r := range b.Records {
			if sc, ok := r.(*SegCommit); ok {
				commit := acceptedCommit{unit: uint32(sc.Partition), epoch: sc.Epoch, writer: b.Writer, seq: sc.Seq}
				if tab.HoldsEpoch(DomainCrawlPart, uint32(sc.Partition), b.Writer, sc.Epoch) {
					accepted = append(accepted, commit)
				} else {
					rejected = append(rejected, commit)
				}
			}
			tab.applyRecord(r)
		}
	}
	// Replay via a fresh handle so the test exercises the same path a
	// booting node would.
	if _, err := Open(context.Background(), c.s3, Options{Prefix: c.opt.Prefix, Writer: 99, Incarnation: 1, Observe: fold}); err != nil {
		t.Fatal(err)
	}
	return accepted, rejected
}

// TestFenceZombieWalkthrough is the doc 02 section 5 walkthrough as code:
// node A holds partition 7, stalls past its deadline, node B takes epoch+1
// and commits; A wakes and commits at its dead epoch. The fold must accept
// exactly B's commit, and A's own discipline helpers must show A should
// never have written at all.
func TestFenceZombieWalkthrough(t *testing.T) {
	client, _ := fakeBucket(t)
	ctx := context.Background()

	tableA := NewLeaseTable()
	a, err := Open(ctx, client, Options{Prefix: "db/", Writer: 1, Incarnation: 1,
		Observe: func(seq uint64, b *Batch) { tableA.Apply(b) }})
	if err != nil {
		t.Fatal(err)
	}

	// t=0: A claims partition 7 at epoch 4 (prior epochs already burned).
	// A's own append folds into its table through Observe.
	grantA := &LeaseGrant{Epoch: 4, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 60_000}
	if _, err := a.Append(ctx, []Record{grantA}); err != nil {
		t.Fatal(err)
	}
	if l, _ := tableA.Get(DomainCrawlPart, 7); l.Epoch != 4 || l.Node != 1 {
		t.Fatalf("A's own grant did not fold: %+v", l)
	}

	// A stalls for 90 seconds. B arrives at t=90s, folds the chain, sees the
	// lease expired, takes epoch 5, and commits segment 12.
	tableB := NewLeaseTable()
	b, err := Open(ctx, client, Options{Prefix: "db/", Writer: 2, Incarnation: 1,
		Observe: func(seq uint64, batch *Batch) { tableB.Apply(batch) }})
	if err != nil {
		t.Fatal(err)
	}
	nowB := uint64(90_000)
	epoch, ok := tableB.Claimable(DomainCrawlPart, 7, nowB)
	if !ok || epoch != 5 {
		t.Fatalf("taker discipline: epoch %d ok %v", epoch, ok)
	}
	if _, err := b.Append(ctx, []Record{
		&LeaseGrant{Epoch: 5, Domain: DomainCrawlPart, Unit: 7, Node: 2, DeadlineMS: nowB + TTLCrawlMS},
		&SegCommit{Epoch: 5, Family: FamilyPage, Partition: 7, Seq: 12},
	}); err != nil {
		t.Fatal(err)
	}

	// A wakes at t=90s with its stale table. Discipline says: suspended.
	leaseA, _ := tableA.Get(DomainCrawlPart, 7)
	if !leaseA.MustSuspend(90_000) {
		t.Fatal("a disciplined A would have self-suspended")
	}
	// A misbehaves anyway and appends its own segment-12 commit at epoch 4.
	// The append itself lands (the chain is not the fence), and A's view
	// advances through the loss, revealing B's take.
	if _, err := a.Append(ctx, []Record{&SegCommit{Epoch: 4, Family: FamilyPage, Partition: 7, Seq: 12}}); err != nil {
		t.Fatal(err)
	}
	if l, _ := tableA.Get(DomainCrawlPart, 7); l.Epoch != 5 || l.Node != 2 {
		t.Fatalf("A's fold missed B's take: %+v", l)
	}

	accepted, rejected := replayWithFence(t, a)
	if len(accepted) != 1 || accepted[0].writer != 2 || accepted[0].epoch != 5 {
		t.Fatalf("accepted: %+v", accepted)
	}
	if len(rejected) != 1 || rejected[0].writer != 1 || rejected[0].epoch != 4 {
		t.Fatalf("rejected: %+v", rejected)
	}
}

// simNode is one participant in the randomized torture: its lease table and
// clock freeze whenever it skips syncing, which is exactly how real zombies
// are made.
type simNode struct {
	ch    *Chain
	table *LeaseTable
	seen  uint64 // the node's frozen view of the clock
}

// TestFenceTorture runs many nodes over shared units with stalls, lazy
// rounds, and clock jumps, then replays the chain once and checks the
// invariants everything downstream stands on: per unit, accepted commits
// have monotone epochs, one writer per epoch, and every zombie write landed
// in the rejected pile.
func TestFenceTorture(t *testing.T) {
	client, _ := fakeBucket(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(2107))

	const (
		nodes  = 5
		units  = 4
		rounds = 300
	)

	sims := make([]*simNode, nodes)
	for i := range nodes {
		s := &simNode{table: NewLeaseTable()}
		ch, err := Open(ctx, client, Options{Prefix: "db/", Writer: uint64(i + 1), Incarnation: 1,
			Observe: func(seq uint64, b *Batch) { s.table.Apply(b) }})
		if err != nil {
			t.Fatal(err)
		}
		s.ch = ch
		sims[i] = s
	}

	now := uint64(1)
	segSeq := uint64(0)
	for range rounds {
		now += uint64(rng.Intn(20_000)) // clock jumps up to 20s between actions
		s := sims[rng.Intn(nodes)]
		me := s.ch.opt.Writer

		// A lazy round acts on a frozen view: no poll, no clock update. This
		// is the stalled-VM case from the walkthrough.
		lazy := rng.Intn(100) < 35
		if !lazy {
			if err := s.ch.Poll(ctx); err != nil {
				t.Fatal(err)
			}
			s.seen = now
		}

		var recs []Record
		for u := range uint32(units) {
			l, held := s.table.Get(DomainCrawlPart, u)
			switch {
			case held && l.Node == me && !l.Released:
				// Holder path, judged on the node's own (possibly stale)
				// clock: suspended holders stay silent, live ones commit and
				// renew at half-life.
				if l.MustSuspend(s.seen) {
					continue
				}
				segSeq++
				recs = append(recs, &SegCommit{Epoch: l.Epoch, Family: FamilyPage, Partition: uint16(u), Seq: segSeq})
				if l.RenewalDue(s.seen) {
					recs = append(recs, &LeaseGrant{Epoch: l.Epoch, Domain: DomainCrawlPart, Unit: u, Node: me, DeadlineMS: s.seen + TTLCrawlMS})
				}
			case rng.Intn(100) < 50:
				if epoch, ok := s.table.Claimable(DomainCrawlPart, u, s.seen); ok {
					recs = append(recs, &LeaseGrant{Epoch: epoch, Domain: DomainCrawlPart, Unit: u, Node: me, DeadlineMS: s.seen + TTLCrawlMS})
				}
			}
		}
		if len(recs) == 0 {
			continue
		}
		if _, err := s.ch.Append(ctx, recs); err != nil {
			t.Fatal(err)
		}
	}

	accepted, rejected := replayWithFence(t, sims[0].ch)
	if len(accepted) == 0 {
		t.Fatal("torture produced no accepted commits")
	}
	if len(rejected) == 0 {
		t.Fatal("torture produced no zombies; the harness is not torturing")
	}
	t.Logf("commits: %d accepted, %d rejected", len(accepted), len(rejected))

	lastEpoch := map[uint32]uint32{}
	epochWriter := map[[2]uint32]uint64{}
	for _, c := range accepted {
		if c.epoch < lastEpoch[c.unit] {
			t.Fatalf("unit %d: accepted epoch went backward: %+v", c.unit, c)
		}
		lastEpoch[c.unit] = c.epoch
		k := [2]uint32{c.unit, c.epoch}
		if w, ok := epochWriter[k]; ok && w != c.writer {
			t.Fatalf("unit %d epoch %d: two writers %d and %d both accepted", c.unit, c.epoch, w, c.writer)
		}
		epochWriter[k] = c.writer
	}
}
