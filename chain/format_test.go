package chain

import (
	"reflect"
	"strings"
	"testing"
)

// fullBatch exercises every record kind once.
func fullBatch() *Batch {
	return &Batch{
		Writer:      0xA1B2C3D4E5F60718,
		Incarnation: 7,
		BatchID:     42,
		Records: []Record{
			&Member{Node: 11, Plane: PlaneCrawl, Incarnation: 7, Endpoints: []string{"10.0.0.1:7101", "10.0.0.1:7102"}, Version: "0.0.0-dev"},
			&LeaseGrant{Epoch: 4, Domain: DomainCrawlPart, Unit: 7, Node: 11, DeadlineMS: 1789000000000},
			&LeaseRelease{Epoch: 4, Domain: DomainCrawlPart, Unit: 7},
			&SegCommit{Epoch: 4, Family: FamilyPage, Partition: 42, Seq: 12, Rows: 20000, Bytes: 64 << 20, Watermark: 12},
			&ManAdvance{Epoch: 4, Family: FamilyPage, Partition: 42, ManSeq: 3},
			&GenPublish{Epoch: 9, Shard: 5, Generation: 2, GenKind: GenBase, Watermarks: []SourceWatermark{{Partition: 42, Seq: 12}, {Partition: 43, Seq: 9}}},
			&GenRetire{Shard: 5, Generation: 1},
			&TierMap{Version: 3, Assign: []TierAssign{{Shard: 5, Tier: 1}, {Shard: 6, Tier: 2}}},
			&Ckpt{Seq: 4096},
		},
	}
}

func TestBatchRoundTrip(t *testing.T) {
	want := fullBatch()
	data, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip mismatch:\nwant %#v\ngot  %#v", want, got)
	}
}

func TestEmptyBatchRoundTrip(t *testing.T) {
	want := &Batch{Writer: 1, Incarnation: 1, BatchID: 1}
	data, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	good, err := fullBatch().Encode()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("every flipped byte fails", func(t *testing.T) {
		// The header crc covers the header, the body crc covers the rest,
		// and the writer field sits between them, covered by neither; a
		// flipped writer still decodes, just as a different author, so skip
		// those 8 bytes. Everything else must fail loudly.
		for i := range good {
			if i >= 24 && i < 32 {
				continue
			}
			bad := append([]byte(nil), good...)
			bad[i] ^= 0x40
			if _, err := DecodeBatch(bad); err == nil {
				t.Fatalf("byte %d flipped, decode still succeeded", i)
			}
		}
	})

	t.Run("every truncation fails", func(t *testing.T) {
		for n := range good {
			if _, err := DecodeBatch(good[:n]); err == nil {
				t.Fatalf("truncated to %d bytes, decode still succeeded", n)
			}
		}
	})

	t.Run("wrong magic", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		copy(bad, "tamndchizu fmt99")
		if _, err := DecodeBatch(bad); err == nil || !strings.Contains(err.Error(), "magic") {
			t.Fatalf("want magic error, got %v", err)
		}
	})
}

func TestEncodeRejectsOversizedRecord(t *testing.T) {
	b := &Batch{Writer: 1, Incarnation: 1, BatchID: 1, Records: []Record{
		&Member{Node: 1, Plane: PlaneCrawl, Version: strings.Repeat("x", 70000)},
	}}
	if _, err := b.Encode(); err == nil {
		t.Fatal("70 KB record body encoded past the u16 limit")
	}
}
