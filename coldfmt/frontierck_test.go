package coldfmt

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
)

func frontierFixture(t *testing.T) ([]byte, map[uint16][]byte) {
	t.Helper()
	rng := rand.New(rand.NewSource(2107))
	ledger := make([]FetchLedgerRow, 300)
	for i := range ledger {
		rng.Read(ledger[i].URLFP[:])
		ledger[i].TS = 1_700_000_000_000 + uint64(i)
		ledger[i].Status = 200
		ledger[i].Bytes = uint32(rng.Intn(100_000))
		ledger[i].ElapsedMS = uint32(rng.Intn(5000))
		ledger[i].Outcome = byte(i % 6)
	}
	sections := map[uint16][]byte{
		SectionURLTableDelta: bytes.Repeat([]byte("url-table-delta "), 500),
		SectionHostQueues:    bytes.Repeat([]byte("hostqueue-state "), 200),
		SectionPending:       bytes.Repeat([]byte{0xAB, 0xCD}, 1000),
		SectionFetchLedger:   AppendFetchLedger(nil, ledger),
	}
	w := &FrontierWriter{Partition: 7, Epoch: 3, Seq: 42, Writer: 9}
	for _, id := range []uint16{SectionURLTableDelta, SectionHostQueues, SectionPending, SectionFetchLedger} {
		w.AddSection(id, sections[id])
	}
	data, err := w.Seal()
	if err != nil {
		t.Fatal(err)
	}
	return data, sections
}

func TestFrontierRoundTrip(t *testing.T) {
	data, sections := frontierFixture(t)
	c, err := OpenFrontierCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Partition != 7 || c.Epoch != 3 || c.Seq != 42 || c.Header.Writer != 9 {
		t.Fatalf("meta: %+v", c)
	}
	if len(c.SectionIDs()) != 4 {
		t.Fatalf("section ids: %v", c.SectionIDs())
	}
	for id, want := range sections {
		got, ok, err := c.Section(id)
		if err != nil || !ok {
			t.Fatalf("section %d: ok=%v err=%v", id, ok, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("section %d differs", id)
		}
	}
	if _, ok, _ := c.Section(SectionURLTableFull); ok {
		t.Fatal("absent section reported present")
	}
}

func TestFrontierRejectsDuplicateSection(t *testing.T) {
	w := &FrontierWriter{}
	w.AddSection(SectionPending, []byte("a"))
	w.AddSection(SectionPending, []byte("b"))
	if _, err := w.Seal(); err == nil {
		t.Fatal("duplicate section sealed")
	}
	if _, err := (&FrontierWriter{}).Seal(); err == nil {
		t.Fatal("empty checkpoint sealed")
	}
}

func TestFetchLedgerRoundTrip(t *testing.T) {
	rows := []FetchLedgerRow{
		{TS: 1, Status: 200, Bytes: 1234, ElapsedMS: 56, Outcome: 2},
		{TS: 2, Status: 404, Bytes: 0, ElapsedMS: 7, Outcome: 1},
	}
	rows[0].URLFP[0] = 0x11
	rows[1].URLFP[15] = 0x99
	got, err := DecodeFetchLedger(AppendFetchLedger(nil, rows))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("got %+v want %+v", got, rows)
	}
	if _, err := DecodeFetchLedger(make([]byte, fetchLedgerRowSize+1)); err == nil {
		t.Fatal("ragged ledger accepted")
	}
}

func TestFrontierFlipSweep(t *testing.T) {
	good, sections := frontierFixture(t)
	// No dictionary in this format, so only the writer field may accept a
	// flip; everything else is covered by the header crc, a section crc,
	// or the footer crc.
	for i := 0; i < len(good); i += 5 {
		bad := bytes.Clone(good)
		bad[i] ^= 0x20
		c, err := OpenFrontierCheckpoint(bad)
		if err != nil {
			continue
		}
		clean := true
		for id, want := range sections {
			got, ok, err := c.Section(id)
			if err != nil || !ok {
				clean = false
				break
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("flip at %d changed section %d without an error", i, id)
			}
		}
		c.Close()
		if clean && (i < 24 || i >= 32) {
			t.Fatalf("flip at %d accepted silently", i)
		}
	}
}

func FuzzOpenFrontierCheckpoint(f *testing.F) {
	w := &FrontierWriter{Partition: 1, Epoch: 1, Seq: 1, Writer: 1}
	w.AddSection(SectionPending, []byte("pending"))
	w.AddSection(SectionFetchLedger, AppendFetchLedger(nil, []FetchLedgerRow{{TS: 1}}))
	data, err := w.Seal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := OpenFrontierCheckpoint(data)
		if err != nil {
			return
		}
		defer c.Close()
		for _, id := range c.SectionIDs() {
			raw, ok, err := c.Section(id)
			if err != nil {
				return
			}
			if !ok {
				t.Fatal("listed section reported absent")
			}
			if id == SectionFetchLedger {
				_, _ = DecodeFetchLedger(raw)
			}
		}
	})
}
