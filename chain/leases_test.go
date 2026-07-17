package chain

import "testing"

func foldRecords(recs ...Record) *LeaseTable {
	t := NewLeaseTable()
	t.Apply(&Batch{Writer: 0, Incarnation: 1, BatchID: 1, Records: recs})
	return t
}

func TestLeaseFoldGrantRenewTakeRelease(t *testing.T) {
	tab := foldRecords(
		&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 60_000},
	)
	l, ok := tab.Get(DomainCrawlPart, 7)
	if !ok || l.Node != 1 || l.Epoch != 1 || l.DeadlineMS != 60_000 {
		t.Fatalf("after grant: %+v %v", l, ok)
	}

	// Renewal by the holder moves the deadline forward only.
	tab.Apply(&Batch{Records: []Record{&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 90_000}}})
	if l, _ = tab.Get(DomainCrawlPart, 7); l.DeadlineMS != 90_000 {
		t.Fatalf("renewal did not extend: %+v", l)
	}
	tab.Apply(&Batch{Records: []Record{&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 80_000}}})
	if l, _ = tab.Get(DomainCrawlPart, 7); l.DeadlineMS != 90_000 {
		t.Fatalf("deadline moved backward: %+v", l)
	}

	// A stranger reusing the sitting epoch is ignored.
	tab.Apply(&Batch{Records: []Record{&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 2, DeadlineMS: 999_000}}})
	if l, _ = tab.Get(DomainCrawlPart, 7); l.Node != 1 || l.DeadlineMS != 90_000 {
		t.Fatalf("stranger stole the epoch: %+v", l)
	}

	// A take at epoch+1 changes ownership.
	tab.Apply(&Batch{Records: []Record{&LeaseGrant{Epoch: 2, Domain: DomainCrawlPart, Unit: 7, Node: 2, DeadlineMS: 150_000}}})
	if l, _ = tab.Get(DomainCrawlPart, 7); l.Node != 2 || l.Epoch != 2 {
		t.Fatalf("take failed: %+v", l)
	}

	// The old holder's late renewal at the burned epoch is ignored.
	tab.Apply(&Batch{Records: []Record{&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 999_000}}})
	if l, _ = tab.Get(DomainCrawlPart, 7); l.Node != 2 || l.Epoch != 2 || l.DeadlineMS != 150_000 {
		t.Fatalf("zombie renewal applied: %+v", l)
	}

	// Release burns the epoch without freeing it for reuse.
	tab.Apply(&Batch{Records: []Record{&LeaseRelease{Epoch: 2, Domain: DomainCrawlPart, Unit: 7}}})
	if l, _ = tab.Get(DomainCrawlPart, 7); !l.Released {
		t.Fatalf("release ignored: %+v", l)
	}
	if e, ok := tab.Claimable(DomainCrawlPart, 7, 0); !ok || e != 3 {
		t.Fatalf("claim after release: epoch %d ok %v", e, ok)
	}
}

func TestLeaseFoldStaleReleaseIgnored(t *testing.T) {
	tab := foldRecords(
		&LeaseGrant{Epoch: 2, Domain: DomainCrawlPart, Unit: 7, Node: 2, DeadlineMS: 100},
		&LeaseRelease{Epoch: 1, Domain: DomainCrawlPart, Unit: 7},
	)
	if l, _ := tab.Get(DomainCrawlPart, 7); l.Released {
		t.Fatalf("stale release applied: %+v", l)
	}
}

func TestLeaseEpochsAreDomainScoped(t *testing.T) {
	tab := foldRecords(
		&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 100},
		&LeaseGrant{Epoch: 1, Domain: DomainBuildJob, Unit: 7, Node: 2, DeadlineMS: 100},
	)
	a, _ := tab.Get(DomainCrawlPart, 7)
	b, _ := tab.Get(DomainBuildJob, 7)
	if a.Node != 1 || b.Node != 2 {
		t.Fatalf("domains collided: %+v %+v", a, b)
	}
}

func TestHolderDiscipline(t *testing.T) {
	l := Lease{Domain: DomainCrawlPart, Node: 1, Epoch: 1, DeadlineMS: 100_000}
	// Half-life renewal: crawl TTL 60s, so renewal is due from 70s on.
	if l.RenewalDue(69_000) {
		t.Fatal("renewal due before half-life")
	}
	if !l.RenewalDue(70_000) {
		t.Fatal("renewal not due at half-life")
	}
	// Self-suspension inside the slack window.
	if l.MustSuspend(94_000) {
		t.Fatal("suspended too early")
	}
	if !l.MustSuspend(95_000) {
		t.Fatal("kept writing inside the slack window")
	}
	if l.Expired(100_000) {
		t.Fatal("expired at the deadline, not after it")
	}
	if !l.Expired(100_001) {
		t.Fatal("not expired past the deadline")
	}
}

func TestClaimable(t *testing.T) {
	tab := NewLeaseTable()
	if e, ok := tab.Claimable(DomainCrawlPart, 7, 0); !ok || e != 1 {
		t.Fatalf("virgin unit: epoch %d ok %v", e, ok)
	}
	tab.Apply(&Batch{Records: []Record{&LeaseGrant{Epoch: 1, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 60_000}}})
	if _, ok := tab.Claimable(DomainCrawlPart, 7, 30_000); ok {
		t.Fatal("claimed a live lease")
	}
	if e, ok := tab.Claimable(DomainCrawlPart, 7, 60_001); !ok || e != 2 {
		t.Fatalf("expired unit: epoch %d ok %v", e, ok)
	}
}

func TestHoldsEpochFence(t *testing.T) {
	tab := foldRecords(&LeaseGrant{Epoch: 4, Domain: DomainCrawlPart, Unit: 7, Node: 1, DeadlineMS: 100})
	if !tab.HoldsEpoch(DomainCrawlPart, 7, 1, 4) {
		t.Fatal("holder rejected")
	}
	if tab.HoldsEpoch(DomainCrawlPart, 7, 1, 3) {
		t.Fatal("stale epoch accepted")
	}
	if tab.HoldsEpoch(DomainCrawlPart, 7, 2, 4) {
		t.Fatal("wrong node accepted")
	}
	tab.Apply(&Batch{Records: []Record{&LeaseRelease{Epoch: 4, Domain: DomainCrawlPart, Unit: 7}}})
	if tab.HoldsEpoch(DomainCrawlPart, 7, 1, 4) {
		t.Fatal("released lease still fences as held")
	}
}
