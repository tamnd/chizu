package coldfmt

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Doc 04 section 1: a block's comp byte selects how its payload is stored.
// rawlen is always the decoded size so readers allocate once.
const (
	CompStored   byte = 0
	CompZstd     byte = 1
	CompZstdDict byte = 2
)

// blockHeaderSize is rawlen u32, storedlen u32, comp u8, reserved 3.
const blockHeaderSize = 12

// maxBlockRawLen caps a declared decoded size, so a corrupt or hostile
// header cannot demand an arbitrary allocation. The biggest honest block is
// one raw chunk of a column, far under this.
const maxBlockRawLen = 1 << 30

// blockCodec compresses and decompresses column blocks for one segment.
// The dictionary is per segment (doc 04 section 3), so the codec is too.
type blockCodec struct {
	dict     []byte
	encPlain *zstd.Encoder
	encDict  *zstd.Encoder // nil when the segment has no dictionary
	dec      *zstd.Decoder
}

func newBlockCodec(dict []byte) (*blockCodec, error) {
	encPlain, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	c := &blockCodec{dict: dict, encPlain: encPlain}
	if len(dict) > 0 {
		c.encDict, err = zstd.NewWriter(nil, zstd.WithEncoderDict(dict))
		if err != nil {
			return nil, err
		}
		c.dec, err = zstd.NewReader(nil, zstd.WithDecoderDicts(dict))
	} else {
		c.dec, err = zstd.NewReader(nil)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *blockCodec) close() {
	c.encPlain.Close()
	if c.encDict != nil {
		c.encDict.Close()
	}
	c.dec.Close()
}

// appendBlock frames raw as one column block, compressed per want unless
// stored is smaller, in which case comp falls back to 0: fixed-width
// columns routinely do not pay for zstd. Returns the extended buffer and
// the stored payload length for the caller's colindex entry.
func (c *blockCodec) appendBlock(dst, raw []byte, want byte) ([]byte, uint32) {
	comp := want
	var payload []byte
	switch want {
	case CompZstdDict:
		if c.encDict != nil {
			payload = c.encDict.EncodeAll(raw, nil)
			break
		}
		comp = CompZstd
		fallthrough
	case CompZstd:
		payload = c.encPlain.EncodeAll(raw, nil)
	default:
		comp = CompStored
	}
	if comp != CompStored && len(payload) >= len(raw) {
		comp = CompStored
	}
	if comp == CompStored {
		payload = raw
	}
	start := len(dst)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(raw)))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(payload)))
	dst = append(dst, comp, 0, 0, 0)
	dst = append(dst, payload...)
	return binary.LittleEndian.AppendUint32(dst, CRC(dst[start:])), uint32(len(payload))
}

// decodeBlock verifies and decodes one block occupying exactly b.
func (c *blockCodec) decodeBlock(b []byte) ([]byte, error) {
	if len(b) < blockHeaderSize+4 {
		return nil, errors.New("coldfmt: block too short")
	}
	if binary.LittleEndian.Uint32(b[len(b)-4:]) != CRC(b[:len(b)-4]) {
		return nil, errors.New("coldfmt: block crc mismatch")
	}
	rawlen := binary.LittleEndian.Uint32(b)
	storedlen := binary.LittleEndian.Uint32(b[4:])
	comp := b[8]
	if rawlen > maxBlockRawLen {
		return nil, fmt.Errorf("coldfmt: block rawlen %d exceeds cap", rawlen)
	}
	if int(storedlen) != len(b)-blockHeaderSize-4 {
		return nil, errors.New("coldfmt: block storedlen mismatch")
	}
	payload := b[blockHeaderSize : len(b)-4]
	switch comp {
	case CompStored:
		if rawlen != storedlen {
			return nil, errors.New("coldfmt: stored block rawlen mismatch")
		}
		return payload, nil
	case CompZstd, CompZstdDict:
		raw, err := c.dec.DecodeAll(payload, make([]byte, 0, rawlen))
		if err != nil {
			return nil, fmt.Errorf("coldfmt: block decompress: %w", err)
		}
		if uint32(len(raw)) != rawlen {
			return nil, errors.New("coldfmt: block rawlen mismatch")
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("coldfmt: unknown comp %d", comp)
	}
}
