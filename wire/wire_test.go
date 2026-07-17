package wire

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func mustFrame(t *testing.T, kind byte, reqid uint64, body []byte) []byte {
	t.Helper()
	data, err := AppendFrame(nil, kind, reqid, body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFrameRoundTrip(t *testing.T) {
	body := bytes.Repeat([]byte("payload "), 1000) // 8 KB, grows the reader buffer
	data := mustFrame(t, KindQResult, 42, body)
	f, n, err := ParseFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) || f.Kind != KindQResult || f.Reqid != 42 || !bytes.Equal(f.Body, body) {
		t.Fatalf("frame drifted: kind=%d reqid=%d n=%d", f.Kind, f.Reqid, n)
	}
}

func TestFrameReaderStream(t *testing.T) {
	var stream []byte
	stream = append(stream, mustFrame(t, KindPing, 1, nil)...)
	stream = append(stream, mustFrame(t, KindQuery, 2, bytes.Repeat([]byte("q"), 8192))...)
	stream = append(stream, mustFrame(t, KindCancel, 3, nil)...)

	fr := NewFrameReader(bytes.NewReader(stream))
	kinds := []byte{KindPing, KindQuery, KindCancel}
	reqids := []uint64{1, 2, 3}
	for i := range kinds {
		f, err := fr.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Kind != kinds[i] || f.Reqid != reqids[i] {
			t.Fatalf("frame %d: kind=%d reqid=%d", i, f.Kind, f.Reqid)
		}
	}
	if _, err := fr.Next(); err != io.EOF {
		t.Fatalf("stream end: %v", err)
	}
}

func TestFrameRejects(t *testing.T) {
	if _, err := AppendFrame(nil, KindQuery, 1, make([]byte, MaxFrame)); err == nil {
		t.Error("oversized body accepted")
	}
	if _, _, err := ParseFrame([]byte{1, 2}); err == nil {
		t.Error("short prefix accepted")
	}
	under := binary.LittleEndian.AppendUint32(nil, 8) // below kind+reqid
	if _, _, err := ParseFrame(append(under, make([]byte, 8)...)); err == nil {
		t.Error("under-length frame accepted")
	}
	over := binary.LittleEndian.AppendUint32(nil, MaxFrame+1)
	if _, _, err := ParseFrame(append(over, make([]byte, 16)...)); err == nil {
		t.Error("over-length frame accepted")
	}
	data := mustFrame(t, KindPing, 1, nil)
	if _, _, err := ParseFrame(data[:len(data)-1]); err == nil {
		t.Error("truncated frame accepted")
	}
	fr := NewFrameReader(bytes.NewReader(data[:len(data)-1]))
	if _, err := fr.Next(); err == nil {
		t.Error("reader accepted truncated frame")
	}
	fr = NewFrameReader(bytes.NewReader(over))
	if _, err := fr.Next(); err == nil {
		t.Error("reader accepted oversized declared length")
	}
}

func TestDecodeBodyShapes(t *testing.T) {
	for _, kind := range []byte{KindPing, KindBusy, KindCancel} {
		if _, err := DecodeBody(Frame{Kind: kind}); err != nil {
			t.Errorf("kind %d empty body rejected: %v", kind, err)
		}
		if _, err := DecodeBody(Frame{Kind: kind, Body: []byte{1}}); err == nil {
			t.Errorf("kind %d with body accepted", kind)
		}
	}
	if _, err := DecodeBody(Frame{Kind: 200}); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, err := DecodeBody(Frame{Kind: KindQuery, Body: []byte{1, 2}}); err == nil {
		t.Error("garbage query body accepted")
	}
}

// FuzzParseFrame is the wire-fuzz envelope target: any accepted frame
// must re-encode to exactly the consumed bytes, and its body must
// either parse per its kind or reject cleanly.
func FuzzParseFrame(f *testing.F) {
	seed := func(kind byte, body []byte) {
		if data, err := AppendFrame(nil, kind, 7, body); err == nil {
			f.Add(data)
		}
	}
	seed(KindPing, nil)
	if b, err := AppendHello(nil, &Hello{Version: 1, NodeID: 9, MaxInflight: 64, MaxFrame: MaxFrame}); err == nil {
		seed(KindHello, b)
	}
	if b, err := AppendQuery(nil, fixtureQuery()); err == nil {
		seed(KindQuery, b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, n, err := ParseFrame(data)
		if err != nil {
			return
		}
		out, err := AppendFrame(nil, fr.Kind, fr.Reqid, fr.Body)
		if err != nil {
			t.Fatalf("re-encode of accepted frame: %v", err)
		}
		if !bytes.Equal(out, data[:n]) {
			t.Fatal("accepted frame is not canonical")
		}
		// The body parser must never panic; errors are fine.
		_, _ = DecodeBody(fr)
	})
}
