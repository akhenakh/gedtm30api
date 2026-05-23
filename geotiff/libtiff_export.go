package geotiff

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"io"
	"sync"
	"unsafe"
)

var (
	remoteReader   io.ReaderAt
	remoteReaderMu sync.Mutex
)

//export goRemoteRead
func goRemoteRead(buf unsafe.Pointer, size C.int64_t, offset C.int64_t) C.int64_t {
	remoteReaderMu.Lock()
	r := remoteReader
	remoteReaderMu.Unlock()

	if r == nil {
		return -1
	}
	n, err := r.ReadAt(unsafe.Slice((*byte)(buf), int(size)), int64(offset))
	if err != nil && err != io.EOF {
		return -1
	}
	return C.int64_t(n)
}
