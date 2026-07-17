// Package wire owns the bytes of the internal RPC protocol of spec
// 2107 doc 02 section 8: a minimal length-prefixed binary framing over
// TCP between root and serve nodes, multiplexed by reqid. Everything
// is stdlib and fixed-width little endian; there is no schema
// compiler and no retry logic here, because the root owns hedging and
// replica choice.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrame bounds the declared frame length (kind + reqid + body), the
// fuzz-gated sanity check that stops a corrupt length prefix from
// allocating gigabytes.
const MaxFrame = 16 << 20

// frameOverhead is the fixed part after the length prefix: kind u8 and
// reqid u64.
const frameOverhead = 9

// Frame kinds. BUSY is a whole answer, not an error code: a serve node
// over its in-flight cap rejects a QUERY immediately and the root
// treats that as an instant hedge trigger. CANCEL kills the loser of a
// hedge by reqid.
const (
	KindHello      byte = 1
	KindPing       byte = 2
	KindQuery      byte = 3
	KindQResult    byte = 4
	KindSnip       byte = 5
	KindSnipResult byte = 6
	KindBusy       byte = 7
	KindCancel     byte = 8
)

// Frame is one decoded frame. Body aliases the reader's buffer and is
// valid until the next read.
type Frame struct {
	Kind  byte
	Reqid uint64
	Body  []byte
}

// AppendFrame encodes one frame: len u32 over kind+reqid+body, then
// the three fields.
func AppendFrame(dst []byte, kind byte, reqid uint64, body []byte) ([]byte, error) {
	if len(body) > MaxFrame-frameOverhead {
		return nil, fmt.Errorf("wire: %d-byte body exceeds the frame bound", len(body))
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(frameOverhead+len(body)))
	dst = append(dst, kind)
	dst = binary.LittleEndian.AppendUint64(dst, reqid)
	return append(dst, body...), nil
}

// ParseFrame decodes the frame at the front of data and returns the
// bytes consumed. It checks only the envelope; DecodeBody checks the
// kind's body shape.
func ParseFrame(data []byte) (Frame, int, error) {
	if len(data) < 4 {
		return Frame{}, 0, errors.New("wire: short frame prefix")
	}
	l := binary.LittleEndian.Uint32(data)
	if l < frameOverhead || l > MaxFrame {
		return Frame{}, 0, fmt.Errorf("wire: declared frame length %d out of bounds", l)
	}
	if uint64(len(data)) < 4+uint64(l) {
		return Frame{}, 0, errors.New("wire: truncated frame")
	}
	f := Frame{
		Kind:  data[4],
		Reqid: binary.LittleEndian.Uint64(data[5:]),
		Body:  data[13 : 4+l],
	}
	return f, int(4 + l), nil
}

// FrameReader reads frames off a long-lived connection, reusing one
// buffer. The connection's reader goroutine owns it; frames dispatch
// to per-request workers by reqid.
type FrameReader struct {
	r   io.Reader
	buf []byte
}

func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r, buf: make([]byte, 4096)}
}

// Next reads one frame. The returned Body aliases the internal buffer
// and is valid until the next call.
func (fr *FrameReader) Next() (Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(fr.r, prefix[:]); err != nil {
		return Frame{}, err
	}
	l := binary.LittleEndian.Uint32(prefix[:])
	if l < frameOverhead || l > MaxFrame {
		return Frame{}, fmt.Errorf("wire: declared frame length %d out of bounds", l)
	}
	if uint64(len(fr.buf)) < uint64(l) {
		fr.buf = make([]byte, l)
	}
	b := fr.buf[:l]
	if _, err := io.ReadFull(fr.r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{
		Kind:  b[0],
		Reqid: binary.LittleEndian.Uint64(b[1:]),
		Body:  b[frameOverhead:],
	}, nil
}

// DecodeBody parses a frame's body per its kind. PING, BUSY, and
// CANCEL carry no body and reject one; unknown kinds are protocol
// errors, never skipped, because both ends of this wire ship in the
// same binary.
func DecodeBody(f Frame) (any, error) {
	switch f.Kind {
	case KindHello:
		return ParseHello(f.Body)
	case KindQuery:
		return ParseQuery(f.Body)
	case KindQResult:
		return ParseQResult(f.Body)
	case KindSnip:
		return ParseSnip(f.Body)
	case KindSnipResult:
		return ParseSnipResult(f.Body)
	case KindPing, KindBusy, KindCancel:
		if len(f.Body) != 0 {
			return nil, fmt.Errorf("wire: kind %d frame with a body", f.Kind)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("wire: unknown frame kind %d", f.Kind)
	}
}
