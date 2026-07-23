package serve

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// plannerFile writes n bytes where byte i is a function of i, so any
// completed read verifies against its offset alone.
func plannerFile(t *testing.T, n int) *os.File {
	t.Helper()
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*7 + i>>9)
	}
	path := filepath.Join(t.TempDir(), "blocks.dat")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func wantBlock(off int64, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		j := off + int64(i)
		b[i] = byte(j*7 + j>>9)
	}
	return b
}

func TestPlannerBatch(t *testing.T) {
	f := plannerFile(t, 1<<20)
	pool := NewBufPool(ReadUnit)
	p := NewPlanner(f, pool, 8)
	defer p.Close()

	// Three frontier batches back to back, larger than the depth and
	// scattered, tags route each completion to its request.
	var batch []BlockReq
	want := map[uint32][]byte{}
	for i := range uint32(48) {
		off := int64((i * 20011) % (1<<20 - ReadUnit))
		batch = append(batch, BlockReq{Off: off, Len: ReadUnit, Tag: i})
		want[i] = wantBlock(off, ReadUnit)
	}
	p.Issue(batch)

	seen := 0
	for {
		c, ok := p.Next()
		if !ok {
			break
		}
		if c.Err != nil {
			t.Fatalf("tag %d: %v", c.Req.Tag, c.Err)
		}
		if !bytes.Equal(c.Buf, want[c.Req.Tag]) {
			t.Fatalf("tag %d: wrong bytes", c.Req.Tag)
		}
		seen++
		p.Release(c)
	}
	if seen != 48 {
		t.Fatalf("completions %d, want 48", seen)
	}
	if s := p.Stats(); s.Reads != 48 || s.Bytes != 48*ReadUnit {
		t.Fatalf("stats %+v", s)
	}
}

func TestPlannerShortTail(t *testing.T) {
	size := ReadUnit + 100
	f := plannerFile(t, size)
	pool := NewBufPool(ReadUnit)
	p := NewPlanner(f, pool, 2)
	defer p.Close()

	p.Issue([]BlockReq{{Off: ReadUnit, Len: ReadUnit, Tag: 1}})
	c, ok := p.Next()
	if !ok {
		t.Fatal("no completion")
	}
	if c.Err == nil || len(c.Buf) != 100 {
		t.Fatalf("tail read: err=%v len=%d, want short-read error with 100 bytes", c.Err, len(c.Buf))
	}
	if !bytes.Equal(c.Buf, wantBlock(int64(ReadUnit), 100)) {
		t.Fatal("tail bytes wrong")
	}
	p.Release(c)
	if _, ok := p.Next(); ok {
		t.Fatal("phantom completion")
	}
}

func TestPlannerExactEOF(t *testing.T) {
	f := plannerFile(t, 2*ReadUnit)
	pool := NewBufPool(ReadUnit)
	p := NewPlanner(f, pool, 2)
	defer p.Close()

	p.Issue([]BlockReq{{Off: ReadUnit, Len: ReadUnit}})
	c, _ := p.Next()
	if c.Err != nil || len(c.Buf) != ReadUnit {
		t.Fatalf("read ending at EOF: err=%v len=%d", c.Err, len(c.Buf))
	}
	p.Release(c)
}

func TestPlannerOversizeRequest(t *testing.T) {
	f := plannerFile(t, 1<<20)
	pool := NewBufPool(ReadUnit)
	p := NewPlanner(f, pool, 2)
	defer p.Close()

	p.Issue([]BlockReq{{Off: 0, Len: pool.Size() + 1, Tag: 9}})
	c, ok := p.Next()
	if !ok || c.Err == nil || c.Req.Tag != 9 {
		t.Fatalf("oversize request: ok=%v err=%v", ok, c.Err)
	}
	if _, ok := p.Next(); ok {
		t.Fatal("planner not drained")
	}
}

// countingReader proves reads run concurrently: it tracks the high
// water mark of simultaneous ReadAt calls.
type countingReader struct {
	f        *os.File
	cur, max atomic.Int32
}

func (c *countingReader) ReadAt(b []byte, off int64) (int, error) {
	n := c.cur.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	defer c.cur.Add(-1)
	return c.f.ReadAt(b, off)
}

func TestPlannerReachesDepth(t *testing.T) {
	cr := &countingReader{f: plannerFile(t, 1<<22)}
	pool := NewBufPool(ReadUnit)
	p := NewPlanner(cr, pool, 8)
	defer p.Close()

	var batch []BlockReq
	for i := range uint32(256) {
		batch = append(batch, BlockReq{Off: int64(i) * ReadUnit % (1<<22 - ReadUnit), Len: ReadUnit, Tag: i})
	}
	p.Issue(batch)
	for {
		c, ok := p.Next()
		if !ok {
			break
		}
		p.Release(c)
	}
	// Page-cache reads are fast enough that hitting the full depth is
	// timing luck; more than one concurrent read proves the pool shape.
	if got := cr.max.Load(); got < 2 {
		t.Fatalf("max concurrent reads %d, want at least 2", got)
	}
}
