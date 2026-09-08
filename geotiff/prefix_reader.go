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
//
// The default (64 KiB) comfortably covers typical COG headers; if a file's IFD
// is larger, readTags transparently falls back to extra network reads (visible
// as a non-zero prefix_misses in the open log) and the value can be raised via
// SetHeaderPrefetchSize.
var headerPrefetchSize atomic.Int64

func init() { headerPrefetchSize.Store(64 * 1024) }

// SetHeaderPrefetchSize sets how many header bytes are prefetched on open.
// Values < 1 leave the current setting unchanged.
func SetHeaderPrefetchSize(n int64) {
	if n >= 1 {
		headerPrefetchSize.Store(n)
	}
}

// prefixReader wraps a reader and serves reads that fall within a pre-fetched
// header block (or one of the additional extents added via addExtent) from
// memory, delegating everything else (tile data) to the underlying reader.
// It implements io.ReadSeeker and io.ReaderAt.
//
// The prefix and extents are populated during GeoTIFF/VRT source setup
// (OpenWithCache and its callees), before any concurrent tile read can
// start, so ReadAt can read them lock-free; only the sequential Read/Seek
// offset is mutex-protected (it is used single-threaded during parsing).
type prefixReader struct {
	inner  io.ReaderAt
	prefix []byte
	size   int64

	// extra holds additional byte ranges fetched after the initial prefix --
	// e.g. IFD tag values (TileOffsets/TileByteCounts and similar) that
	// readTags found outside the header prefix and batch-fetched in one
	// round trip via prefetchMissingRanges, instead of one network read per
	// missed tag. Populated once before concurrent reads begin (see above),
	// so no locking is needed to read it.
	extra []prefixExtent

	mu  sync.Mutex
	off int64

	fallbacks int64 // atomic: reads that missed the prefix and all extents
}

// prefixExtent is one additional cached byte range beyond the initial
// prefix.
type prefixExtent struct {
	off  int64
	data []byte
}

// addExtent records an additional byte range as servable from memory. Not
// safe for concurrent use with itself or with ReadAt; callers must only use
// it during single-threaded source setup (see prefixReader doc comment).
func (p *prefixReader) addExtent(off int64, data []byte) {
	if len(data) == 0 {
		return
	}
	p.extra = append(p.extra, prefixExtent{off: off, data: data})
}

// covers reports whether [start, end) is already fully served from memory
// (the initial prefix or a previously added extent).
func (p *prefixReader) covers(start, end int64) bool {
	if start >= 0 && end <= int64(len(p.prefix)) {
		return true
	}
	for _, e := range p.extra {
		if start >= e.off && end <= e.off+int64(len(e.data)) {
			return true
		}
	}
	return false
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

	// Issue the header prefetch *before* asking for the file size. For a
	// reader that only learns its size from a read (BlobReader), this lets
	// the very first range read double as size discovery, instead of
	// requiring a separate size lookup (an S3 HeadObject) before any data is
	// read at all. Readers that already know their size for free (a local
	// file, an HTTP HEAD done at construction) are unaffected either way.
	n := headerPrefetchSize.Load()
	var prefix []byte
	if n > 0 {
		buf := make([]byte, n)
		read, err := parallelReadAt(ra, buf, 0)
		if err != nil && err != io.EOF {
			return nil, err
		}
		prefix = buf[:read]
	}

	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if int64(len(prefix)) > size {
		prefix = prefix[:size]
	}

	return &prefixReader{inner: ra, prefix: prefix, size: size}, nil
}

func (p *prefixReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= 0 && off+int64(len(b)) <= int64(len(p.prefix)) {
		return copy(b, p.prefix[off:off+int64(len(b))]), nil
	}
	for _, e := range p.extra {
		if off >= e.off && off+int64(len(b)) <= e.off+int64(len(e.data)) {
			start := off - e.off
			return copy(b, e.data[start:start+int64(len(b))]), nil
		}
	}
	// A read outside the prefetched prefix and every added extent means a
	// network round-trip; counting these reveals whether the header/IFD
	// prefetch (and its follow-up merged fetch) failed to cover the IFD.
	atomic.AddInt64(&p.fallbacks, 1)
	return p.inner.ReadAt(b, off)
}

// PrefixLen returns the number of bytes prefetched from the start of the file.
func (p *prefixReader) PrefixLen() int { return len(p.prefix) }

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
