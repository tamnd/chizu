package crawl

import (
	"context"
	"fmt"

	"github.com/tamnd/chizu/chain"
	"github.com/tamnd/chizu/s3c"
)

// BucketSink lands segments the doc 04 way: upload to
// cold/page/p<pppp>/<seq16>.cold, then SegCommit on the chain. The
// object exists only once the commit record is appended, which is what
// makes a crashed import re-runnable: an orphan upload with no commit
// is invisible and GC's problem, never a duplicate row.
type BucketSink struct {
	Client *s3c.Client
	Chain  *chain.Chain
	Prefix string
	Epoch  uint32
}

func (s *BucketSink) Commit(ctx context.Context, meta SegMeta, data []byte) error {
	key := fmt.Sprintf("%scold/page/p%04d/%016d.cold", s.Prefix, meta.Partition, meta.Seq)
	if _, err := s.Client.Put(ctx, key, data); err != nil {
		return err
	}
	_, err := s.Chain.Append(ctx, []chain.Record{&chain.SegCommit{
		Epoch:     s.Epoch,
		Family:    meta.Family,
		Partition: meta.Partition,
		Seq:       meta.Seq,
		Rows:      meta.Rows,
		Bytes:     meta.Bytes,
		Watermark: meta.Watermark,
	}})
	return err
}
