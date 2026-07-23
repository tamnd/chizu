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
	return uploadHot(ctx, c, key, path, partSize, false)
}

// UploadHotConsume is UploadHot for a source the caller is done with:
// each part's disk space is hole-punched once the bucket acknowledges
// it, so the local and remote copies never coexist in full. The file
// is garbage afterward; the caller removes it.
func UploadHotConsume(ctx context.Context, c *s3c.Client, key, path string, partSize int) error {
	return uploadHot(ctx, c, key, path, partSize, true)
}

func uploadHot(ctx context.Context, c *s3c.Client, key, path string, partSize int, consume bool) error {
	if partSize < minPartSize {
		return errors.New("build: part size below the S3 minimum")
	}
	mode := os.O_RDONLY
	if consume {
		mode = os.O_RDWR
	}
	f, err := os.OpenFile(path, mode, 0)
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
	var off int64
	punch := consume
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
		if punch {
			if err := punchHole(f.Fd(), off, int64(k)); err != nil {
				punch = false
			}
		}
		off += int64(k)
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
