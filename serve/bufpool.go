// Pooled aligned pread buffers, the read discipline of doc 05 section
// 11: all postings, skip, positions, and doc band reads go through
// these, never mmap. Buffers are 4 KiB aligned so the pool works
// unchanged if O_DIRECT ever earns its way in, and pooled so the
// steady query path allocates nothing (doc 07 section 9).

package serve

import (
	"sync"
	"unsafe"
)

const bufAlign = 4096

// BufPool hands out fixed-size aligned buffers.
type BufPool struct {
	size int
	pool sync.Pool
}

// NewBufPool builds a pool of size-byte buffers, size rounded up to
// the 4 KiB alignment quantum.
func NewBufPool(size int) *BufPool {
	size = (size + bufAlign - 1) &^ (bufAlign - 1)
	p := &BufPool{size: size}
	p.pool.New = func() any { return alignedBuf(size) }
	return p
}

// Get returns an aligned buffer of the pool's full size.
func (p *BufPool) Get() []byte { return p.pool.Get().([]byte) }

// Put returns a buffer to the pool. Foreign or resliced buffers are
// dropped rather than poisoning the pool.
func (p *BufPool) Put(b []byte) {
	if len(b) != p.size || !aligned(b) {
		return
	}
	p.pool.Put(b) //nolint:staticcheck // the slice header allocation is amortized by reuse
}

// Size reports the pool's buffer size after rounding.
func (p *BufPool) Size() int { return p.size }

// alignedBuf carves an aligned window out of an oversized allocation.
func alignedBuf(size int) []byte {
	raw := make([]byte, size+bufAlign)
	off := 0
	if r := int(uintptr(unsafe.Pointer(unsafe.SliceData(raw))) & (bufAlign - 1)); r != 0 {
		off = bufAlign - r
	}
	return raw[off : off+size : off+size]
}

func aligned(b []byte) bool {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))&(bufAlign-1) == 0
}
