package chain

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tamnd/chizu/s3c"
)

func testRoot() *Root {
	return &Root{
		Writer:    11,
		DBID:      0xDEADBEEFCAFEF00D,
		CreatedMS: 1789000000000,
		P:         4096,
		ShardSize: 6000000,
		Frozen:    []byte("law=1 tok=1 quant=sq8"),
		CkptSeq:   0,
	}
}

func TestRootRoundTrip(t *testing.T) {
	want := testRoot()
	data, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRoot(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip mismatch:\nwant %#v\ngot  %#v", want, got)
	}
}

func TestRootRejectsCorruption(t *testing.T) {
	good, err := testRoot().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for i := range good {
		if i >= 24 && i < 32 {
			continue // the writer field, covered by neither crc
		}
		bad := append([]byte(nil), good...)
		bad[i] ^= 0x40
		if _, err := DecodeRoot(bad); err == nil {
			t.Fatalf("byte %d flipped, decode still succeeded", i)
		}
	}
	for n := range good {
		if _, err := DecodeRoot(good[:n]); err == nil {
			t.Fatalf("truncated to %d bytes, decode still succeeded", n)
		}
	}
	// A batch is not a root.
	batch, err := fullBatch().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRoot(batch); err == nil {
		t.Fatal("decoded a chain batch as a root")
	}
}

func FuzzDecodeRoot(f *testing.F) {
	if seed, err := testRoot().Encode(); err == nil {
		f.Add(seed)
		f.Add(seed[:len(seed)/2])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := DecodeRoot(data)
		if err != nil {
			return
		}
		out, err := r.Encode()
		if err != nil {
			t.Fatalf("decoded root failed to re-encode: %v", err)
		}
		r2, err := DecodeRoot(out)
		if err != nil {
			t.Fatalf("re-encoded root failed to decode: %v", err)
		}
		if !reflect.DeepEqual(r, r2) {
			t.Fatalf("round trip drift:\nfirst  %#v\nsecond %#v", r, r2)
		}
	})
}

// rootStoreSuite runs the same lifecycle against either discovery path.
func rootStoreSuite(t *testing.T, seqFallback bool) {
	c, st := fakeBucket(t)
	ctx := context.Background()

	a := NewRootStore(c, "db/", 1, seqFallback)
	if _, err := a.Load(ctx); !errors.Is(err, s3c.ErrNotFound) {
		t.Fatalf("load before create: want ErrNotFound, got %v", err)
	}
	if err := a.Create(ctx, testRoot()); err != nil {
		t.Fatal(err)
	}
	if err := a.Create(ctx, testRoot()); !errors.Is(err, s3c.ErrPrecondition) {
		t.Fatalf("second create: want ErrPrecondition, got %v", err)
	}

	got, err := a.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.DBID != testRoot().DBID || got.CkptSeq != 0 {
		t.Fatalf("loaded root: %+v", got)
	}

	// Advance moves forward and is a no-op when already there or past.
	if r, err := a.Advance(ctx, 4096); err != nil || r.CkptSeq != 4096 {
		t.Fatalf("advance: %+v %v", r, err)
	}
	putsAfterAdvance := st.puts.Load()
	if r, err := a.Advance(ctx, 100); err != nil || r.CkptSeq != 4096 {
		t.Fatalf("stale advance: %+v %v", r, err)
	}
	if st.puts.Load() != putsAfterAdvance {
		t.Fatal("stale advance wrote")
	}

	// A second, independent store discovers the current root and can race.
	b := NewRootStore(c, "db/", 2, seqFallback)
	if r, err := b.Load(ctx); err != nil || r.CkptSeq != 4096 {
		t.Fatalf("second store load: %+v %v", r, err)
	}
	if r, err := b.Advance(ctx, 8192); err != nil || r.CkptSeq != 8192 {
		t.Fatalf("second store advance: %+v %v", r, err)
	}
	// a's cached state is now stale; its next advance must converge anyway.
	if r, err := a.Advance(ctx, 8192); err != nil || r.CkptSeq != 8192 || r.Writer != 2 {
		t.Fatalf("stale-handle advance: %+v %v", r, err)
	}
	if r, err := a.Advance(ctx, 12288); err != nil || r.CkptSeq != 12288 || r.Writer != 1 {
		t.Fatalf("advance past racer: %+v %v", r, err)
	}

	// Frozen fields survive every advance.
	final, err := b.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := testRoot()
	if final.DBID != want.DBID || final.P != want.P || final.ShardSize != want.ShardSize || string(final.Frozen) != string(want.Frozen) {
		t.Fatalf("frozen fields drifted: %+v", final)
	}
}

func TestRootStoreCAS(t *testing.T) { rootStoreSuite(t, false) }
func TestRootStoreSeq(t *testing.T) { rootStoreSuite(t, true) }

func TestRootSeqLeavesDenseSequence(t *testing.T) {
	c, st := fakeBucket(t)
	ctx := context.Background()
	s := NewRootStore(c, "db/", 1, true)
	if err := s.Create(ctx, testRoot()); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := s.Advance(ctx, uint64(i+1)*100); err != nil {
			t.Fatal(err)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, key := range []string{
		"db/rootv/0000000000000000",
		"db/rootv/0000000000000001",
		"db/rootv/0000000000000002",
		"db/rootv/0000000000000003",
	} {
		if _, ok := st.objects[key]; !ok {
			t.Fatalf("missing %s; objects: %d", key, len(st.objects))
		}
	}
	if len(st.objects) != 4 {
		t.Fatalf("want exactly 4 rootv objects, have %d", len(st.objects))
	}
}
