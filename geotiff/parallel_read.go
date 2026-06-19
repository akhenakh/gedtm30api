package geotiff

import (
	"io"
	"sync"
	"sync/atomic"
)

// tileFetchChunkSize is the granularity at which a large read is split into
// concurrent range requests.
const tileFetchChunkSize = 256 * 1024

// tileFetchConcurrency caps how many concurrent range requests a single large
// read may issue. A single TCP stream to object storage rarely saturates a WAN
// link, so fetching a tile as several parallel ranges aggregates throughput.
var tileFetchConcurrency int64 = 4

// SetTileFetchConcurrency sets the maximum number of concurrent range requests
// used to fetch one tile/header. Values < 1 disable parallelism.
func SetTileFetchConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	atomic.StoreInt64(&tileFetchConcurrency, int64(n))
}

// parallelReadAt fills buf from ra starting at off. For reads larger than one
// chunk it issues up to tileFetchConcurrency concurrent range reads over
// disjoint sub-slices (safe: each goroutine writes its own region and the
// readers' ReadAt is stateless). Small reads go straight through.
func parallelReadAt(ra io.ReaderAt, buf []byte, off int64) (int, error) {
	conc := int(atomic.LoadInt64(&tileFetchConcurrency))
	n := len(buf)
	if conc <= 1 || n <= tileFetchChunkSize {
		return ra.ReadAt(buf, off)
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for start := 0; start < n; start += tileFetchChunkSize {
		end := start + tileFetchChunkSize
		if end > n {
			end = n
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(s, e int) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := ra.ReadAt(buf[s:e], off+int64(s)); err != nil && err != io.EOF {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(start, end)
	}
	wg.Wait()

	if firstErr != nil {
		return 0, firstErr
	}
	return n, nil
}
