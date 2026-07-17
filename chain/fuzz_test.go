package chain

import (
	"reflect"
	"testing"
)

// FuzzDecodeBatch is the doc 10 format-fuzz posture for the chain parser:
// arbitrary bytes must produce a clean error or a batch that survives a
// canonical re-encode, never a panic and never silent drift.
func FuzzDecodeBatch(f *testing.F) {
	if seed, err := fullBatch().Encode(); err == nil {
		f.Add(seed)
		short := append([]byte(nil), seed...)
		f.Add(short[:len(short)/2])
		flipped := append([]byte(nil), seed...)
		flipped[40] ^= 0xFF
		f.Add(flipped)
	}
	if seed, err := (&Batch{Writer: 1, Incarnation: 1, BatchID: 1}).Encode(); err == nil {
		f.Add(seed)
	}
	f.Add([]byte("tamndchizu fmt01"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := DecodeBatch(data)
		if err != nil {
			return
		}
		out, err := b.Encode()
		if err != nil {
			t.Fatalf("decoded batch failed to re-encode: %v", err)
		}
		b2, err := DecodeBatch(out)
		if err != nil {
			t.Fatalf("re-encoded batch failed to decode: %v", err)
		}
		if !reflect.DeepEqual(b, b2) {
			t.Fatalf("round trip drift:\nfirst  %#v\nsecond %#v", b, b2)
		}
	})
}
