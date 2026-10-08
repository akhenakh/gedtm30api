package geotiff

import (
	"io"
	"sync"
	"sync/atomic"
)

// tileFetchChunkSize is the granularity at which a large read is split into
// concurrent range requests. It defaults well above a typical single COG
// tile (a few hundred KB to low single-digit MB) so ordinary tile fetches
// stay as one GetObject: inside a single AWS region, per-request latency
// (TTFB) dominates over per-connection throughput for reads this size, so
// splitting them trades one round trip for several without a real gain.
// Splitting still helps for genuinely large reads (e.g. a merged header/IFD
// fetch spanning a wide gap, or an unusually large tile).
var tileFetchChunkSize int64 = 2 * 1024 * 1024

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

// SetTileFetchChunkSize sets the read size above which parallelReadAt splits
// a read into concurrent range requests. Values < 1 leave the current
// setting unchanged.
func SetTileFetchChunkSize(n int64) {
	if n >= 1 {
		atomic.StoreInt64(&tileFetchChunkSize, n)
	}
}

// parallelReadAt fills buf from ra starting at off. For reads larger than one
// chunk it issues up to tileFetchConcurrency concurrent range reads over
// disjoint sub-slices (safe: each goroutine writes its own region and the
// readers' ReadAt is stateless). Small reads go straight through.
func parallelReadAt(ra io.ReaderAt, buf []byte, off int64) (int, error) {
	conc := int(atomic.LoadInt64(&tileFetchConcurrency))
	chunkSize := int(atomic.LoadInt64(&tileFetchChunkSize))
	n := len(buf)
	if conc <= 1 || n <= chunkSize {
		return ra.ReadAt(buf, off)
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for start := 0; start < n; start += chunkSize {
		end := min(start+chunkSize, n)
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
