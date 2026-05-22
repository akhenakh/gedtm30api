//go:build !cgo

package geotiff

import (
	"fmt"
	"io"
)

func getFilePath(r io.ReadSeeker) string {
	return ""
}

func libtiffDecodeTile(path string, tileNum int) ([]byte, error) {
	return nil, fmt.Errorf("libtiff not available (CGo disabled)")
}
