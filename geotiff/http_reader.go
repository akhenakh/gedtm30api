package geotiff

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HTTPRangeReader satisfies the io.ReadSeeker and io.ReaderAt interfaces
// for remote files over HTTP.
type HTTPRangeReader struct {
	url    string
	client *http.Client
	size   int64

	// mu protects the offset field for sequential Read/Seek operations.
	mu     sync.Mutex
	offset int64
}

// NewHTTPRangeReader creates a new reader for a remote file URL.
func NewHTTPRangeReader(url string, client *http.Client) (*HTTPRangeReader, error) {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create head request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http head request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status for http head request: %s", resp.Status)
	}

	if resp.Header.Get("Accept-Ranges") != "bytes" {
		return nil, errors.New("server does not accept byte range requests")
	}

	size := resp.ContentLength
	if size <= 0 {
		return nil, fmt.Errorf("could not determine content length or file is empty")
	}

	return &HTTPRangeReader{
		url:    url,
		client: client,
		size:   size,
	}, nil
}

// Read performs a sequential read. It is safe for concurrent use with other
// sequential reads or seeks, but it is not efficient for concurrent access.
// The lock is held for the entire duration of the network request.
func (h *HTTPRangeReader) Read(p []byte) (n int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.offset >= h.size {
		return 0, io.EOF
	}

	n, err = h.readAt(p, h.offset)
	if n > 0 {
		h.offset += int64(n)
	}
	return n, err
}

// Seek updates the internal offset for the next sequential Read.
func (h *HTTPRangeReader) Seek(offset int64, whence int) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = h.offset + offset
	case io.SeekEnd:
		newOffset = h.size + offset
	default:
		return 0, errors.New("invalid whence")
	}

	if newOffset < 0 {
		return 0, errors.New("cannot seek to negative offset")
	}
	h.offset = newOffset
	return h.offset, nil
}

// ReadAt implements io.ReaderAt for concurrent, stateless reads. This is the
// method used for fetching tiles and it is highly performant. It does NOT use
// the mutex and does not affect the internal offset.
func (h *HTTPRangeReader) ReadAt(p []byte, off int64) (n int, err error) {
	return h.readAt(p, off)
}

// readAt is the underlying stateless read implementation.
func (h *HTTPRangeReader) readAt(p []byte, off int64) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("http.readAt: invalid offset %d", off)
	}
	if off >= h.size {
		return 0, io.EOF
	}

	bytesToRead := int64(len(p))
	if off+bytesToRead > h.size {
		bytesToRead = h.size - off
	}

	req, err := http.NewRequest(http.MethodGet, h.url, nil)
	if err != nil {
		return 0, err
	}

	rangeEnd := off + bytesToRead - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, rangeEnd))

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("expected status 206 Partial Content, got: %s", resp.Status)
	}

	return io.ReadFull(resp.Body, p[:bytesToRead])
}
