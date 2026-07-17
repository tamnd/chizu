package chain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/tamnd/chizu/s3c"
)

// mixRecords returns a varied, deterministic record mix so folds exercise
// every checkpoint section.
func mixRecords(i int) []Record {
	u := uint32(i % 3)
	epoch := uint32(i/3 + 1)
	recs := []Record{
		&Member{Node: uint64(i%4 + 1), Plane: PlaneCrawl, Incarnation: uint32(i), Endpoints: []string{fmt.Sprintf("10.0.0.%d:7070", i%4+1)}, Version: "v0.1"},
		&LeaseGrant{Epoch: epoch, Domain: DomainCrawlPart, Unit: u, Node: uint64(i%4 + 1), DeadlineMS: uint64(i+1) * 10_000},
		&SegCommit{Epoch: epoch, Family: FamilyPage, Partition: uint16(u), Seq: uint64(i), Rows: 100, Bytes: 1000, Watermark: uint64(i)},
		&ManAdvance{Epoch: epoch, Family: FamilyPage, Partition: uint16(u), ManSeq: uint64(i + 1)},
		&GenPublish{Epoch: 1, Shard: uint16(i % 5), Generation: uint64(i), GenKind: GenBase, Watermarks: []SourceWatermark{{Partition: 1, Seq: uint64(i)}}},
	}
	if i >= 4 && i%4 == 0 {
		recs = append(recs, &GenRetire{Shard: uint16((i - 4) % 5), Generation: uint64(i - 4)})
	}
	if i%5 == 0 {
		recs = append(recs, &TierMap{Version: uint64(i/5 + 1), Assign: []TierAssign{{Shard: 1, Tier: 1}, {Shard: 2, Tier: byte(i%2 + 1)}}})
	}
	if i%7 == 3 {
		recs = append(recs, &LeaseRelease{Epoch: epoch, Domain: DomainCrawlPart, Unit: u})
	}
	return recs
}

func TestCheckpointRoundTrip(t *testing.T) {
	s := NewState()
	for i := range 20 {
		s.Apply(&Batch{Writer: 1, Records: mixRecords(i)})
	}
	s.CkptSeq = 7

	data := s.EncodeCheckpoint(3, 15)
	got, seq, writer, err := DecodeCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 15 || writer != 3 {
		t.Fatalf("seq %d writer %d", seq, writer)
	}
	if !reflect.DeepEqual(s, got) {
		t.Fatalf("state did not round-trip:\n%+v\n%+v", s, got)
	}
}

func TestCheckpointFlipEveryByte(t *testing.T) {
	s := NewState()
	for i := range 10 {
		s.Apply(&Batch{Writer: 1, Records: mixRecords(i)})
	}
	data := s.EncodeCheckpoint(3, 7)
	for i := range data {
		if i >= 24 && i < 32 {
			// The writer field is covered by neither crc, the one documented
			// exception shared with batches and roots.
			continue
		}
		mut := append([]byte(nil), data...)
		mut[i] ^= 0x40
		if _, _, _, err := DecodeCheckpoint(mut); err == nil {
			t.Fatalf("flip at byte %d decoded", i)
		}
	}
	for n := range len(data) {
		if _, _, _, err := DecodeCheckpoint(data[:n]); err == nil {
			t.Fatalf("truncation to %d decoded", n)
		}
	}
	// Cross-type: a checkpoint is not a batch and not a root.
	if _, err := DecodeBatch(data); err == nil {
		t.Fatal("checkpoint decoded as batch")
	}
	if _, err := DecodeRoot(data); err == nil {
		t.Fatal("checkpoint decoded as root")
	}
}

// TestCheckpointRacersProduceIdenticalBytes has two nodes cross the same
// boundary independently: the capture bytes must match exactly (writer field
// included, since it names the triggering batch's writer), and both Flush
// calls succeed with exactly one object in the bucket.
func TestCheckpointRacersProduceIdenticalBytes(t *testing.T) {
	client, st := fakeBucket(t)
	ctx := context.Background()

	cpA := NewCheckpointer()
	cpA.Interval = 4
	a, err := Open(ctx, client, Options{Prefix: "db/", Writer: 1, Incarnation: 1, Observe: cpA.Observe})
	if err != nil {
		t.Fatal(err)
	}
	cpB := NewCheckpointer()
	cpB.Interval = 4
	b, err := Open(ctx, client, Options{Prefix: "db/", Writer: 2, Incarnation: 1, Observe: cpB.Observe})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 4 {
		if _, err := a.Append(ctx, []Record{&Ckpt{Seq: uint64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cpA.pending) != 1 || len(cpB.pending) != 1 {
		t.Fatalf("pending: %d and %d", len(cpA.pending), len(cpB.pending))
	}
	if !reflect.DeepEqual(cpA.pending[0], cpB.pending[0]) {
		t.Fatal("racing folds produced different checkpoint bytes")
	}

	seqsA, err := cpA.Flush(ctx, client, "db/")
	if err != nil {
		t.Fatal(err)
	}
	seqsB, err := cpB.Flush(ctx, client, "db/")
	if err != nil {
		t.Fatal(err)
	}
	if len(seqsA) != 1 || seqsA[0] != 3 || len(seqsB) != 1 || seqsB[0] != 3 {
		t.Fatalf("flushed %v and %v", seqsA, seqsB)
	}
	if st.count() != 5 { // 4 chain slots + 1 checkpoint
		t.Fatalf("%d objects in the bucket", st.count())
	}
}

// driveChain appends n mixed batches through one checkpointing handle,
// announcing every checkpoint with a Ckpt record, and returns the handle,
// its checkpointer, and the newest checkpointed slot.
func driveChain(t *testing.T, ctx context.Context, client *s3c.Client, n int) (*Chain, *Checkpointer, uint64) {
	t.Helper()
	cp := NewCheckpointer()
	cp.Interval = 8
	a, err := Open(ctx, client, Options{Prefix: "db/", Writer: 1, Incarnation: 1, Observe: cp.Observe})
	if err != nil {
		t.Fatal(err)
	}
	var newest uint64
	for i := range n {
		if _, err := a.Append(ctx, mixRecords(i)); err != nil {
			t.Fatal(err)
		}
		seqs, err := cp.Flush(ctx, client, "db/")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range seqs {
			if _, err := a.Append(ctx, []Record{&Ckpt{Seq: s}}); err != nil {
				t.Fatal(err)
			}
			newest = s
		}
	}
	if _, err := cp.Flush(ctx, client, "db/"); err != nil {
		t.Fatal(err)
	}
	if newest == 0 {
		t.Fatal("drive crossed no checkpoint boundary")
	}
	return a, cp, newest
}

// TestBootFromCheckpointMatchesFullFold is the boot contract: newest
// checkpoint plus tail replay equals the fold from slot zero, state for
// state and byte for byte at the next boundary.
func TestBootFromCheckpointMatchesFullFold(t *testing.T) {
	client, _ := fakeBucket(t)
	ctx := context.Background()
	a, cpA, newest := driveChain(t, ctx, client, 30)

	data, _, err := client.Get(ctx, ckptKey("db/", newest))
	if err != nil {
		t.Fatal(err)
	}
	st, seq, writer, err := DecodeCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	if seq != newest || writer != 1 {
		t.Fatalf("seq %d writer %d", seq, writer)
	}

	cpB := NewCheckpointer()
	cpB.Interval = 8
	cpB.State = st
	b, err := Open(ctx, client, Options{Prefix: "db/", Writer: 2, Incarnation: 1, From: newest + 1, Observe: cpB.Observe})
	if err != nil {
		t.Fatal(err)
	}
	if b.Pos() != a.Pos() {
		t.Fatalf("boot pos %d, tail pos %d", b.Pos(), a.Pos())
	}
	if !reflect.DeepEqual(cpA.State, cpB.State) {
		t.Fatalf("boot state diverged:\n%+v\n%+v", cpA.State, cpB.State)
	}
	// Any boundary the tail replay crossed must have captured the exact bytes
	// the from-zero fold already wrote.
	for _, p := range cpB.pending {
		stored, _, err := client.Get(ctx, ckptKey("db/", p.seq))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.data, stored) {
			t.Fatalf("boot fold captured different bytes for checkpoint %d", p.seq)
		}
	}
	// And the next checkpoint both folds would write is byte-identical.
	if !reflect.DeepEqual(cpA.State.EncodeCheckpoint(9, 999), cpB.State.EncodeCheckpoint(9, 999)) {
		t.Fatal("next checkpoint would diverge")
	}
}

func TestTrimBehind(t *testing.T) {
	client, _ := fakeBucket(t)
	ctx := context.Background()
	a, _, newest := driveChain(t, ctx, client, 30)
	if newest < 15 {
		t.Fatalf("drive only checkpointed to %d", newest)
	}

	// Below two checkpoints of history there is nothing to trim.
	if err := TrimBehind(ctx, client, "db/", 7, 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Get(ctx, a.key(0)); err != nil {
		t.Fatalf("early trim removed slot 0: %v", err)
	}

	if err := TrimBehind(ctx, client, "db/", 15, 8); err != nil {
		t.Fatal(err)
	}
	for seq := range uint64(8) {
		if _, _, err := client.Get(ctx, a.key(seq)); !errors.Is(err, s3c.ErrNotFound) {
			t.Fatalf("slot %d survived the trim: %v", seq, err)
		}
	}
	if _, _, err := client.Get(ctx, a.key(8)); err != nil {
		t.Fatalf("trim overreached into slot 8: %v", err)
	}
	// Trimming is idempotent.
	if err := TrimBehind(ctx, client, "db/", 15, 8); err != nil {
		t.Fatal(err)
	}

	// A node booting through the newest checkpoint never misses the trimmed
	// slots; a bare probe from zero sees an empty chain, which is why
	// discovery goes through the root.
	st, seq, _, err := func() (*State, uint64, uint64, error) {
		data, _, err := client.Get(ctx, ckptKey("db/", newest))
		if err != nil {
			t.Fatal(err)
		}
		return DecodeCheckpoint(data)
	}()
	if err != nil {
		t.Fatal(err)
	}
	cp := NewCheckpointer()
	cp.Interval = 8
	cp.State = st
	b, err := Open(ctx, client, Options{Prefix: "db/", Writer: 3, Incarnation: 1, From: seq + 1, Observe: cp.Observe})
	if err != nil {
		t.Fatal(err)
	}
	if b.Pos() != a.Pos() {
		t.Fatalf("boot after trim reached %d, tail is %d", b.Pos(), a.Pos())
	}
	blind, err := Open(ctx, client, Options{Prefix: "db/", Writer: 4, Incarnation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if blind.Pos() != 0 {
		t.Fatalf("blind probe from zero found %d", blind.Pos())
	}
}

func FuzzDecodeCheckpoint(f *testing.F) {
	s := NewState()
	for i := range 20 {
		s.Apply(&Batch{Writer: 1, Records: mixRecords(i)})
	}
	f.Add(s.EncodeCheckpoint(3, 15))
	f.Add(NewState().EncodeCheckpoint(0, 0))
	f.Fuzz(func(t *testing.T, data []byte) {
		st, seq, writer, err := DecodeCheckpoint(data)
		if err != nil {
			return
		}
		// Whatever decodes must re-encode canonically and fold back to the
		// same state.
		re := st.EncodeCheckpoint(writer, seq)
		st2, seq2, writer2, err := DecodeCheckpoint(re)
		if err != nil {
			t.Fatalf("re-encode did not decode: %v", err)
		}
		if seq2 != seq || writer2 != writer || !reflect.DeepEqual(st, st2) {
			t.Fatal("re-encode round-trip diverged")
		}
	})
}
