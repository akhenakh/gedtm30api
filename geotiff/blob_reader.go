package geotiff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"gocloud.dev/blob"
)

// BlobReader satisfies io.ReadSeeker and io.ReaderAt interfaces
// for cloud buckets (S3, GCS, Azure, etc.) using gocloud.dev/blob.
type BlobReader struct {
	ctx    context.Context
	bucket *blob.Bucket
	key    string

	// size is the object's total size in bytes, discovered lazily to avoid a
	// blocking Attributes (HeadObject) call before any data has been read.
	// -1 means "not yet known". It is normally populated as a side effect of
	// the first ranged read: even a partial GetObject response carries the
	// object's true full size via the Content-Range header.
	size atomic.Int64

	// resolveOnce guards the fallback path used only if a size is needed
	// before any read has happened (e.g. Seek(0, io.SeekEnd) called first).
	resolveOnce sync.Once
	resolveErr  error

	// mu protects the offset field for sequential Read/Seek operations.
	mu     sync.Mutex
	offset int64
}

// NewBlobReader creates a new reader for a blob in a bucket. It performs no
// I/O up front: the object's size is discovered lazily (see the size field),
// so opening a source costs nothing until it is actually read.
func NewBlobReader(ctx context.Context, bucket *blob.Bucket, key string) (*BlobReader, error) {
	r := &BlobReader{ctx: ctx, bucket: bucket, key: key}
	r.size.Store(-1)
	return r, nil
}

// knownSize returns the object's total size, resolving it with a single
// Attributes (HeadObject) call if no read has discovered it yet.
func (r *BlobReader) knownSize() (int64, error) {
	if s := r.size.Load(); s >= 0 {
		return s, nil
	}
	r.resolveOnce.Do(func() {
		attrs, err := r.bucket.Attributes(r.ctx, r.key)
		if err != nil {
			r.resolveErr = fmt.Errorf("failed to get attributes for key %s: %w", r.key, err)
			return
		}
		r.size.CompareAndSwap(-1, attrs.Size)
	})
	if r.resolveErr != nil {
		return 0, r.resolveErr
	}
	return r.size.Load(), nil
}

// Read performs a sequential read.
func (r *BlobReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	size, err := r.knownSize()
	if err != nil {
		return 0, err
	}
	if r.offset >= size {
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
		size, err := r.knownSize()
		if err != nil {
			return 0, err
		}
		newOffset = size + offset
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

	// If a prior read already discovered the size, keep the exact clamped
	// short-read/EOF behavior callers already rely on.
	if known := r.size.Load(); known >= 0 {
		if off >= known {
			return 0, io.EOF
		}
		if off+int64(len(p)) > known {
			p = p[:known-off]
		}
	}

	// Create a range reader for the specific chunk.
	// gocloud.dev/blob uses offset and length (not end byte).
	// We do not use the implicit buffer in reader options as we want direct control.
	reader, err := r.bucket.NewRangeReader(r.ctx, r.key, off, int64(len(p)), nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create range reader: %w", err)
	}
	defer reader.Close()

	// The response carries the object's true full size (via Content-Range
	// for a partial read), so this doubles as size discovery on the first
	// read -- avoiding the separate blocking Attributes/HeadObject call
	// NewBlobReader used to make before any data was even read.
	r.size.CompareAndSwap(-1, reader.Size())

	n, err = io.ReadFull(reader, p)
	if err == io.ErrUnexpectedEOF {
		// Requested past the true end before size was known: a short read
		// at EOF is normal io.ReaderAt behavior, not an error.
		err = io.EOF
	}
	return n, err
}
