package coldfmt

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func manifestCorpus(nsegs int) *Manifest {
	m := &Manifest{Family: FamilyPage, Partition: 42, Epoch: 7, ManSeq: 19, Watermark: 5_000, Writer: 3}
	for i := range nsegs {
		m.Segs = append(m.Segs, ManifestSeg{
			Seq:     uint64(i*3 + 1),
			Bytes:   uint64(i) * 512 << 20,
			NRows:   uint64(i) * 1_000_000,
			FirstMS: 1_700_000_000_000 + uint64(i),
			LastMS:  1_700_000_100_000 + uint64(i),
			Flags:   uint16(i % 3),
		})
	}
	return m
}

func TestManifestRoundTrip(t *testing.T) {
	for _, nsegs := range []int{0, 1, 1200} {
		want := manifestCorpus(nsegs)
		data, err := EncodeManifest(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseManifest(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Segs) == 0 {
			got.Segs = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("nsegs=%d: %+v != %+v", nsegs, got, want)
		}
	}
}

func TestManifestRejectsBadInput(t *testing.T) {
	if _, err := EncodeManifest(&Manifest{Family: 0}); err == nil {
		t.Fatal("family 0 accepted")
	}
	if _, err := EncodeManifest(&Manifest{Family: FamilyDocmap + 1}); err == nil {
		t.Fatal("family past docmap accepted")
	}
	m := manifestCorpus(3)
	m.Segs[2].Seq = m.Segs[1].Seq
	if _, err := EncodeManifest(m); err == nil {
		t.Fatal("duplicate seg seq accepted")
	}
	m.Segs[2].Seq = m.Segs[1].Seq - 1
	if _, err := EncodeManifest(m); err == nil {
		t.Fatal("regressing seg seq accepted")
	}

	good, err := EncodeManifest(manifestCorpus(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(good[:len(good)-5]); err == nil {
		t.Fatal("truncated manifest accepted")
	}
	extra := append(bytes.Clone(good[:len(good)-4]), 0)
	extra = binary.LittleEndian.AppendUint32(extra, CRC(extra[HeaderSize:]))
	if _, err := ParseManifest(extra); err == nil {
		t.Fatal("ragged segment table accepted")
	}
}

// TestSelectManifestZombie is the doc 04 section 9 scenario: a zombie
// writer's manifest must lose, and its watermark must not raise the bar
// for the successor.
func TestSelectManifestZombie(t *testing.T) {
	// Lease history for partition 42: epoch 5 held chain positions
	// [100,200], epoch 6 held [410,460].
	held := func(epoch uint32, pos uint64) bool {
		switch epoch {
		case 5:
			return pos <= 200
		case 6:
			return pos <= 460
		default:
			return false
		}
	}
	man := func(seq uint64, epoch uint32, wm uint64) *Manifest {
		return &Manifest{Family: FamilyPage, Partition: 42, Epoch: epoch, ManSeq: seq, Watermark: wm}
	}
	m1 := man(1, 5, 150) // valid: epoch 5 held at >=0
	m2 := man(2, 5, 400) // valid: epoch 5 held at >=150
	m3 := man(3, 5, 500) // zombie: epoch 5 never held at >=400
	m4 := man(4, 6, 600) // valid against wm 400, NOT the zombie's 500

	w, err := SelectManifest([]*Manifest{m3, m1, m4, m2}, held)
	if err != nil {
		t.Fatal(err)
	}
	if w != m4 {
		t.Fatalf("winner ManSeq %d, want 4", w.ManSeq)
	}

	// If the zombie's watermark had advanced the requirement to 500,
	// epoch 6 (gone by 461) would fail it and m2 would win; prove the
	// distinction is live by checking epoch 6 against both bars.
	if !held(6, 400) || held(6, 500) {
		t.Fatal("scenario no longer distinguishes the zombie watermark")
	}

	// Zombies only: no winner.
	w, err = SelectManifest([]*Manifest{man(1, 9, 10)}, held)
	if err != nil {
		t.Fatal(err)
	}
	if w != nil {
		t.Fatalf("winner %+v from all-zombie set", w)
	}

	// Mixed partitions are a caller bug.
	other := man(5, 6, 700)
	other.Partition = 43
	if _, err := SelectManifest([]*Manifest{m1, other}, held); err == nil {
		t.Fatal("mixed partitions accepted")
	}
	if w, err := SelectManifest(nil, held); err != nil || w != nil {
		t.Fatalf("empty set: %v %v", w, err)
	}
}

func TestManifestFlipSweep(t *testing.T) {
	want := manifestCorpus(40)
	good, err := EncodeManifest(want)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(good); i += 3 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		got, err := ParseManifest(bad)
		if err != nil {
			continue
		}
		if i < 24 || i >= 32 {
			t.Fatalf("flip at %d accepted silently", i)
		}
		got.Writer = want.Writer
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("flip at %d changed the manifest beyond the writer field", i)
		}
	}
}

func FuzzParseManifest(f *testing.F) {
	for _, nsegs := range []int{0, 5} {
		data, err := EncodeManifest(manifestCorpus(nsegs))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseManifest(data)
		if err != nil {
			return
		}
		out, err := EncodeManifest(m)
		if err != nil {
			t.Fatalf("accepted manifest re-encode failed: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("accepted manifest did not re-encode to the input")
		}
	})
}
