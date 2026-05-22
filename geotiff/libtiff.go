package geotiff

/*
#cgo LDFLAGS: -ltiff
#include <tiffio.h>
#include <stdlib.h>

static int libtiffReadTile(const char* path, uint32_t tileNum, uint8_t** outData, tmsize_t* outSize) {
	TIFF* tif = TIFFOpen(path, "r");
	if (!tif) return -1;

	tmsize_t ts = TIFFTileSize(tif);
	if (ts <= 0) { TIFFClose(tif); return -2; }

	uint8_t* buf = (uint8_t*)malloc(ts);
	if (!buf) { TIFFClose(tif); return -3; }

	tmsize_t result = TIFFReadEncodedTile(tif, tileNum, buf, ts);
	TIFFClose(tif);

	if (result < 0) { free(buf); return -4; }

	*outData = buf;
	*outSize = result;
	return 0;
}
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"unsafe"
)

type fileNamer interface {
	Name() string
}

func getFilePath(r io.ReadSeeker) string {
	if fn, ok := r.(fileNamer); ok {
		return fn.Name()
	}
	if f, ok := r.(*os.File); ok {
		return f.Name()
	}
	return ""
}

func libtiffDecodeTile(path string, tileNum int) ([]byte, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	var outData *C.uint8_t
	var outSize C.tmsize_t

	ret := C.libtiffReadTile(cPath, C.uint32_t(tileNum), &outData, &outSize)
	if ret != 0 {
		return nil, fmt.Errorf("libtiff read tile %d from %s failed: %d", tileNum, path, ret)
	}

	n := int(outSize)
	result := make([]byte, n)
	copy(result, C.GoBytes(unsafe.Pointer(outData), C.int(n)))
	C.free(unsafe.Pointer(outData))

	return result, nil
}
