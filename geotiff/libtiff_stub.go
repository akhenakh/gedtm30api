//go:build !cgo

package geotiff

import (
	"fmt"
	"io"
	"unsafe"
)

func getFilePath(r any) string { return "" }

func libtiffDecodeTile(path string, tileNum int) ([]byte, error) {
	return nil, fmt.Errorf("libtiff not available (CGo disabled)")
}

func openRemoteTIFF(reader io.ReaderAt, fileSize int64) (unsafe.Pointer, error) {
	return nil, fmt.Errorf("libtiff not available (CGo disabled)")
}

func readTileFromHandle(tif unsafe.Pointer, tileNum int) ([]byte, error) {
	return nil, fmt.Errorf("libtiff not available (CGo disabled)")
}
