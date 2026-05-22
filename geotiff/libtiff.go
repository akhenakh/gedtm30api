package geotiff

/*
#cgo LDFLAGS: -ltiff
#include <tiffio.h>
#include <stdlib.h>
#include <string.h>

extern int64_t goRemoteRead(void* buf, int64_t size, int64_t offset);

typedef struct { int64_t size; int64_t pos; } RHandle;

static tmsize_t rhRead(thandle_t h, void* buf, tmsize_t size) {
	RHandle* rh = (RHandle*)h;
	int64_t n = goRemoteRead(buf, size, rh->pos);
	if (n <= 0) { memset(buf, 0, size); return 0; }
	rh->pos += n;
	return (tmsize_t)n;
}
static tmsize_t rhWrite(thandle_t h, void* buf, tmsize_t size) { return -1; }
static toff_t rhSeek(thandle_t h, toff_t off, int whence) {
	RHandle* rh = (RHandle*)h;
	if (whence == SEEK_SET) rh->pos = off;
	else if (whence == SEEK_CUR) rh->pos += off;
	else if (whence == SEEK_END) rh->pos = rh->size + off;
	return rh->pos;
}
static int rhClose(thandle_t h) { free((RHandle*)h); return 0; }
static toff_t rhSize(thandle_t h) { return ((RHandle*)h)->size; }
static int rhMap(thandle_t h, void** pbase, toff_t* psize) { return 0; }
static void rhUnmap(thandle_t h, void* base, toff_t size) {}

// Open a remote TIFF backed by goRemoteRead callbacks.
TIFF* gtl_OpenRemote(int64_t fileSize) {
	RHandle* rh = (RHandle*)malloc(sizeof(RHandle));
	if (!rh) return NULL;
	rh->size = fileSize;
	rh->pos = 0;
	return TIFFClientOpen("remote", "r", (thandle_t)rh,
		rhRead, rhWrite, rhSeek, rhClose, rhSize, rhMap, rhUnmap);
}

// Read a single tile from an already-open TIFF handle.
int gtl_ReadTile(TIFF* tif, uint32_t tileNum, uint8_t** outData, tmsize_t* outSize) {
	tmsize_t ts = TIFFTileSize(tif);
	if (ts <= 0) return -1;
	void* buf = malloc(ts);
	if (!buf) return -2;
	memset(buf, 0, ts);
	tmsize_t result = TIFFReadEncodedTile(tif, tileNum, buf, ts);
	if (result < 0) { free(buf); return -3; }
	*outData = (uint8_t*)buf;
	*outSize = result;
	return 0;
}

// Open a local TIFF file, read a single tile, close.
int gtl_ReadTileFromPath(const char* path, uint32_t tileNum, uint8_t** outData, tmsize_t* outSize) {
	TIFF* tif = TIFFOpen(path, "r");
	if (!tif) return -1;
	int ret = gtl_ReadTile(tif, tileNum, outData, outSize);
	TIFFClose(tif);
	return ret;
}

// Close a TIFF handle opened with gtl_OpenRemote.
void gtl_CloseRemote(TIFF* tif) {
	TIFFClose(tif);
}
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"unsafe"
)

type fileNamer interface{ Name() string }

func getFilePath(r any) string {
	if f, ok := r.(*os.File); ok { return f.Name() }
	if fn, ok := r.(fileNamer); ok { return fn.Name() }
	return ""
}

func libtiffDecodeTile(path string, tileNum int) ([]byte, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	var outData *C.uint8_t
	var outSize C.tmsize_t
	ret := C.gtl_ReadTileFromPath(cPath, C.uint32_t(tileNum), &outData, &outSize)
	if ret != 0 { return nil, fmt.Errorf("libtiff read tile %d from %s failed: %d", tileNum, path, ret) }
	return copyDecompressedData(outData, outSize), nil
}

func openRemoteTIFF(reader io.ReaderAt, fileSize int64) (unsafe.Pointer, error) {
	remoteReaderMu.Lock()
	remoteReader = reader
	remoteReaderMu.Unlock()

	tif := C.gtl_OpenRemote(C.int64_t(fileSize))
	if tif == nil {
		remoteReaderMu.Lock()
		remoteReader = nil
		remoteReaderMu.Unlock()
		return nil, fmt.Errorf("gtl_OpenRemote failed")
	}
	return unsafe.Pointer(tif), nil
}

func readTileFromHandle(tif unsafe.Pointer, tileNum int) ([]byte, error) {
	var outData *C.uint8_t
	var outSize C.tmsize_t
	ret := C.gtl_ReadTile((*C.TIFF)(tif), C.uint32_t(tileNum), &outData, &outSize)
	if ret != 0 { return nil, fmt.Errorf("gtl_ReadTile failed: %d", ret) }
	return copyDecompressedData(outData, outSize), nil
}

func copyDecompressedData(outData *C.uint8_t, outSize C.tmsize_t) []byte {
	n := int(outSize)
	result := make([]byte, n)
	copy(result, C.GoBytes(unsafe.Pointer(outData), C.int(n)))
	C.free(unsafe.Pointer(outData))
	return result
}

func closeRemoteTIFF(tif unsafe.Pointer) {
	C.gtl_CloseRemote((*C.TIFF)(tif))
}
