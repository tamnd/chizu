package build

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/tamnd/chizu/s3c"
)

// DefaultPartSize is the multipart chunk for shard uploads. S3 wants
// at least 5 MB per non-final part; 64 MB keeps an 800 GB shard under
// the 10k part cap with room to spare.
const DefaultPartSize = 64 << 20

const minPartSize = 5 << 20

// UploadHot streams a sealed .hot from disk into the bucket as a
// multipart upload. Parts are read sequentially, so memory stays at
// one part.
func UploadHot(ctx context.Context, c *s3c.Client, key, path string, partSize int) error {
	if partSize < minPartSize {
		return errors.New("build: part size below the S3 minimum")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	uploadID, err := c.MultipartCreate(ctx, key)
	if err != nil {
		return err
	}
	var parts []s3c.CompletedPart
	buf := make([]byte, partSize)
	for n := 1; ; n++ {
		k, rerr := io.ReadFull(f, buf)
		if rerr == io.EOF {
			break
		}
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			return rerr
		}
		etag, perr := c.MultipartPut(ctx, key, uploadID, n, buf[:k])
		if perr != nil {
			return perr
		}
		parts = append(parts, s3c.CompletedPart{PartNumber: n, ETag: etag})
		if rerr == io.ErrUnexpectedEOF {
			break
		}
	}
	if len(parts) == 0 {
		return errors.New("build: empty shard file")
	}
	_, err = c.MultipartComplete(ctx, key, uploadID, parts, false)
	return err
}
