package geotiff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"gocloud.dev/blob"
)

// BlobReader satisfies io.ReadSeeker and io.ReaderAt interfaces
// for cloud buckets (S3, GCS, Azure, etc.) using gocloud.dev/blob.
type BlobReader struct {
	ctx    context.Context
	bucket *blob.Bucket
	key    string
	size   int64

	// mu protects the offset field for sequential Read/Seek operations.
	mu     sync.Mutex
	offset int64
}

// NewBlobReader creates a new reader for a blob in a bucket.
func NewBlobReader(ctx context.Context, bucket *blob.Bucket, key string) (*BlobReader, error) {
	// Get attributes to determine file size and existence.
	attrs, err := bucket.Attributes(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get attributes for key %s: %w", key, err)
	}

	return &BlobReader{
		ctx:    ctx,
		bucket: bucket,
		key:    key,
		size:   attrs.Size,
	}, nil
}

// Read performs a sequential read.
func (r *BlobReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.offset >= r.size {
		return 0, io.EOF
	}

	n, err = r.readAt(p, r.offset)
	if n > 0 {
		r.offset += int64(n)
	}
	return n, err
}

// Seek updates the internal offset for the next sequential Read.
func (r *BlobReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = r.offset + offset
	case io.SeekEnd:
		newOffset = r.size + offset
	default:
		return 0, errors.New("invalid whence")
	}

	if newOffset < 0 {
		return 0, errors.New("cannot seek to negative offset")
	}
	r.offset = newOffset
	return r.offset, nil
}

// ReadAt implements io.ReaderAt for concurrent, stateless reads.
func (r *BlobReader) ReadAt(p []byte, off int64) (n int, err error) {
	return r.readAt(p, off)
}

// readAt is the underlying stateless read implementation.
func (r *BlobReader) readAt(p []byte, off int64) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("blob.readAt: invalid offset %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}

	length := int64(len(p))
	if off+length > r.size {
		length = r.size - off
	}

	// Create a range reader for the specific chunk.
	// gocloud.dev/blob uses offset and length (not end byte).
	// We do not use the implicit buffer in reader options as we want direct control.
	reader, err := r.bucket.NewRangeReader(r.ctx, r.key, off, length, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create range reader: %w", err)
	}
	defer reader.Close()

	// ReadFull ensures we get the exact bytes requested (or until EOF).
	return io.ReadFull(reader, p[:length])
}
