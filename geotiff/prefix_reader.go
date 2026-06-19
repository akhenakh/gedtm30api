package geotiff

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// headerPrefetchSize is how many bytes from the start of a file are fetched in
// one read when a GeoTIFF is opened. A COG's IFD and its TileOffsets/
// TileByteCounts arrays live near the front, so this lets both the Go tag
// parser and libtiff's TIFFClientOpen read the entire header from memory
// instead of issuing one network range request per tag.
const headerPrefetchSize int64 = 512 * 1024

// prefixReader wraps a reader and serves reads that fall within a pre-fetched
// header block from memory, delegating everything else (tile data) to the
// underlying reader. It implements io.ReadSeeker and io.ReaderAt.
//
// The prefix is immutable after construction, so ReadAt is lock-free; only the
// sequential Read/Seek offset is mutex-protected (it is used single-threaded
// during parsing).
type prefixReader struct {
	inner  io.ReaderAt
	prefix []byte
	size   int64

	mu  sync.Mutex
	off int64

	fallbacks int64 // atomic: reads that missed the prefix
}

// fileNamerAt is implemented by readers (e.g. *os.File) that can report a path;
// prefixReader forwards it so getFilePath still routes local files to the
// by-path libtiff decoder.
type fileNamerAt interface{ Name() string }

// newPrefixReader pre-fetches up to headerPrefetchSize bytes from the start of
// r and returns a wrapper serving them from memory. r must implement
// io.ReaderAt and io.Seeker (all of this package's readers do).
func newPrefixReader(r io.ReadSeeker) (*prefixReader, error) {
	ra, ok := r.(io.ReaderAt)
	if !ok {
		return nil, errors.New("reader does not support ReadAt")
	}

	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	n := headerPrefetchSize
	if size < n {
		n = size
	}

	prefix := make([]byte, n)
	if n > 0 {
		read, err := parallelReadAt(ra, prefix, 0)
		if err != nil && err != io.EOF {
			return nil, err
		}
		prefix = prefix[:read]
	}

	return &prefixReader{inner: ra, prefix: prefix, size: size}, nil
}

func (p *prefixReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= 0 && off+int64(len(b)) <= int64(len(p.prefix)) {
		return copy(b, p.prefix[off:off+int64(len(b))]), nil
	}
	// A read outside the prefetched prefix means a network round-trip; counting
	// these reveals whether the prefix is too small to cover the header/IFD.
	atomic.AddInt64(&p.fallbacks, 1)
	return p.inner.ReadAt(b, off)
}

// PrefixLen returns the number of bytes prefetched from the start of the file.
func (p *prefixReader) PrefixLen() int { return len(p.prefix) }

// Size returns the total file size.
func (p *prefixReader) Size() int64 { return p.size }

// Fallbacks returns how many reads missed the prefetched prefix and hit the
// underlying reader (i.e. caused a network round-trip).
func (p *prefixReader) Fallbacks() int64 { return atomic.LoadInt64(&p.fallbacks) }

func (p *prefixReader) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.off >= p.size {
		return 0, io.EOF
	}
	n, err := p.ReadAt(b, p.off)
	p.off += int64(n)
	return n, err
}

func (p *prefixReader) Seek(offset int64, whence int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = p.off + offset
	case io.SeekEnd:
		abs = p.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	p.off = abs
	return abs, nil
}

// Name forwards the underlying file path when available.
func (p *prefixReader) Name() string {
	if fn, ok := p.inner.(fileNamerAt); ok {
		return fn.Name()
	}
	return ""
}
