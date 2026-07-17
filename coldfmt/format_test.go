package coldfmt

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	data := AppendHeader(nil, FormatPageSeg, 42)
	if len(data) != HeaderSize {
		t.Fatalf("header length %d", len(data))
	}
	h, err := ParseHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if h.Format != FormatPageSeg || h.FVersion != FVersion || h.Writer != 42 {
		t.Fatalf("header %+v", h)
	}
}

// Bytes 0..23 are covered by the header crc, so any flip there must reject.
// Bytes 24..31 are the writer field, covered by each object's own trailing
// or footer crc instead; a header-only parse accepts those flips, and that
// exception is deliberate and documented in doc 04 section 2.
func TestHeaderFlipEveryByte(t *testing.T) {
	good := AppendHeader(nil, FormatManifest, 7)
	for i := range good {
		bad := bytes.Clone(good)
		bad[i] ^= 0x40
		_, err := ParseHeader(bad)
		if i < 24 && err == nil {
			t.Fatalf("flip at %d accepted", i)
		}
		if i >= 24 && err != nil {
			t.Fatalf("flip at %d in writer field rejected by header parse: %v", i, err)
		}
	}
	for cut := range good {
		if _, err := ParseHeader(good[:cut]); err == nil {
			t.Fatalf("truncation to %d accepted", cut)
		}
	}
}

func TestHeaderRejectsBadVersion(t *testing.T) {
	for _, v := range []uint16{0, FVersion + 1} {
		data := AppendHeader(nil, FormatRoot, 1)
		binary.LittleEndian.PutUint16(data[18:], v)
		binary.LittleEndian.PutUint32(data[20:], CRC(data[:20]))
		if _, err := ParseHeader(data); err == nil {
			t.Fatalf("fversion %d accepted", v)
		}
	}
}

func TestTailRoundTrip(t *testing.T) {
	tail := AppendTail(nil, 1<<40, 4096)
	if len(tail) != TailSize {
		t.Fatalf("tail length %d", len(tail))
	}
	// ParseTail reads the last 16 bytes, so a whole object works too.
	obj := append(AppendHeader(nil, FormatGraphSeg, 3), tail...)
	for _, in := range [][]byte{tail, obj} {
		off, l, err := ParseTail(in)
		if err != nil {
			t.Fatal(err)
		}
		if off != 1<<40 || l != 4096 {
			t.Fatalf("tail %d %d", off, l)
		}
	}
}

func TestTailFlipEveryByte(t *testing.T) {
	good := AppendTail(nil, 12345, 678)
	for i := range good {
		bad := bytes.Clone(good)
		bad[i] ^= 0x01
		if _, _, err := ParseTail(bad); err == nil {
			t.Fatalf("flip at %d accepted", i)
		}
	}
	if _, _, err := ParseTail(good[:TailSize-1]); err == nil {
		t.Fatal("short tail accepted")
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add(AppendHeader(nil, FormatPageSeg, 42))
	f.Add(AppendHeader(nil, FormatHotShard, 0))
	f.Add([]byte(Magic))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHeader(data)
		if err != nil {
			return
		}
		// Accepted bytes re-encode to the exact input. The writer field is
		// outside the header crc, so the rebuild covers all 32 bytes.
		out := []byte(Magic)
		out = binary.LittleEndian.AppendUint16(out, h.Format)
		out = binary.LittleEndian.AppendUint16(out, h.FVersion)
		out = binary.LittleEndian.AppendUint32(out, CRC(out))
		out = binary.LittleEndian.AppendUint64(out, h.Writer)
		if !bytes.Equal(out, data[:HeaderSize]) {
			t.Fatal("accepted header does not re-encode to input")
		}
	})
}

func FuzzParseTail(f *testing.F) {
	f.Add(AppendTail(nil, 0, 0))
	f.Add(AppendTail(nil, 1<<40, 4096))
	f.Fuzz(func(t *testing.T, data []byte) {
		off, l, err := ParseTail(data)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendTail(nil, off, l), data[len(data)-TailSize:]) {
			t.Fatal("accepted tail does not re-encode to input")
		}
	})
}
