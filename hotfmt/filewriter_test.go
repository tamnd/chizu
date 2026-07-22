package hotfmt

import (
	"bytes"
	"testing"
)

// The streaming writer must produce the exact bytes EncodeFile produces
// for the same contents; the builder depends on that identity.
func TestFileWriterMatchesEncodeFile(t *testing.T) {
	want := fixtureFile(t)

	bands := fixtureBands()
	var buf bytes.Buffer
	fw, err := NewFileWriter(&buf, fixtureHeader(), fixtureMeta(), fixtureStats())
	if err != nil {
		t.Fatal(err)
	}
	// Alternate the streaming and in-memory paths so both are pinned.
	if err := fw.WriteBand(BandDict, bytes.NewReader(bands[BandDict])); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBandBytes(BandDocvalues, bands[BandDocvalues]); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBand(BandTombstones, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBand(BandPostings, bytes.NewReader(bands[BandPostings])); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBandBytes(BandSkips, bands[BandSkips]); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBand(BandPositions, bytes.NewReader(bands[BandPositions])); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBand(BandDocband, bytes.NewReader(bands[BandDocband])); err != nil {
		t.Fatal(err)
	}
	if err := fw.Finish(fixtureProv()); err != nil {
		t.Fatal(err)
	}

	got := buf.Bytes()
	if !bytes.Equal(got, want) {
		if len(got) != len(want) {
			t.Fatalf("length mismatch: got %d want %d", len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("first byte mismatch at offset %d: got %#x want %#x", i, got[i], want[i])
			}
		}
	}

	s, err := openBytes(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyBands(); err != nil {
		t.Fatal(err)
	}
}

func TestFileWriterRejectsOutOfOrder(t *testing.T) {
	var buf bytes.Buffer
	fw, err := NewFileWriter(&buf, fixtureHeader(), fixtureMeta(), fixtureStats())
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteBandBytes(BandPostings, nil); err == nil {
		t.Fatal("postings accepted before dict")
	}
	if err := fw.Finish(fixtureProv()); err == nil {
		t.Fatal("finish accepted with bands missing")
	}
}
