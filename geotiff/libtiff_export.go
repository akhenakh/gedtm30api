package geotiff

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"io"
	"log/slog"
	"sync"
	"unsafe"
)

// remoteEntry holds the I/O context for a single open remote libtiff handle.
//
// reader is the underlying source (used as a fallback). buf/base hold an
// optional pre-fetched byte range: when a libtiff read falls entirely within
// [base, base+len(buf)) it is served from memory, avoiding a network
// round-trip while the per-handle decode lock is held.
type remoteEntry struct {
	reader io.ReaderAt
	buf    []byte
	base   int64
}

var (
	remoteRegistry  = map[uintptr]*remoteEntry{}
	remoteRegistryM sync.Mutex
	remoteIDCounter uintptr
)

// registerRemoteReader records a reader and returns a unique handle id. The id
// is handed to libtiff and passed back to goRemoteRead, so each open handle is
// served by its own reader rather than a single shared global.
func registerRemoteReader(r io.ReaderAt) uintptr {
	remoteRegistryM.Lock()
	defer remoteRegistryM.Unlock()
	remoteIDCounter++
	id := remoteIDCounter
	remoteRegistry[id] = &remoteEntry{reader: r}
	return id
}

func unregisterRemoteReader(id uintptr) {
	remoteRegistryM.Lock()
	delete(remoteRegistry, id)
	remoteRegistryM.Unlock()
}

// setRemoteBuffer attaches a pre-fetched byte range to a handle. It must only
// be called by the goroutine currently holding that handle's decode lock, so
// there is no concurrent mutation of the same entry's buffer.
func setRemoteBuffer(id uintptr, buf []byte, base int64) {
	remoteRegistryM.Lock()
	if e := remoteRegistry[id]; e != nil {
		e.buf = buf
		e.base = base
	}
	remoteRegistryM.Unlock()
}

func clearRemoteBuffer(id uintptr) {
	remoteRegistryM.Lock()
	if e := remoteRegistry[id]; e != nil {
		e.buf = nil
		e.base = 0
	}
	remoteRegistryM.Unlock()
}

//export goRemoteRead
func goRemoteRead(id C.uintptr_t, buf unsafe.Pointer, size C.int64_t, offset C.int64_t) C.int64_t {
	remoteRegistryM.Lock()
	e := remoteRegistry[uintptr(id)]
	remoteRegistryM.Unlock()

	if e == nil || e.reader == nil {
		return -1
	}

	off := int64(offset)
	n := int(size)
	dst := unsafe.Slice((*byte)(buf), n)

	// Serve from the pre-fetched in-memory buffer when the requested range is
	// fully contained. This is the hot path during tile decoding.
	if e.buf != nil && off >= e.base && off+int64(n) <= e.base+int64(len(e.buf)) {
		start := int(off - e.base)
		copy(dst, e.buf[start:start+n])
		slog.Debug("goRemoteRead buffer hit", "offset", off, "size", n)
		return C.int64_t(n)
	}

	// A read while a tile buffer is set but outside its range means libtiff
	// needed bytes we did not pre-fetch, so this falls back to the network
	// *under the decode lock* — the case to watch when decodes look serialized.
	if e.buf != nil {
		slog.Debug("goRemoteRead buffer MISS (network read under decode lock)",
			"offset", off, "size", n, "bufBase", e.base, "bufLen", len(e.buf))
	}

	// Fallback: read directly from the source (e.g. header/IFD access, or a
	// read outside the pre-fetched range).
	rn, err := e.reader.ReadAt(dst, off)
	if err != nil && err != io.EOF {
		return -1
	}
	return C.int64_t(rn)
}
