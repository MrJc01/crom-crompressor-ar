//go:build cgo

package store

// #cgo LDFLAGS: -lz
// #include <zlib.h>
import "C"

import (
	"fmt"
	"unsafe"
)

// zlibCompress comprime com a zlib do sistema (a mesma que o git e quase
// todo programa usam): só ela recria os bytes originais exatamente.
func zlibCompress(src []byte, level int) ([]byte, error) {
	bound := C.compressBound(C.uLong(len(src)))
	dst := make([]byte, int(bound))
	dlen := C.uLongf(bound)
	var sp *C.Bytef
	if len(src) > 0 {
		sp = (*C.Bytef)(unsafe.Pointer(&src[0]))
	} else {
		sp = (*C.Bytef)(unsafe.Pointer(&dst[0]))
	}
	rc := C.compress2((*C.Bytef)(unsafe.Pointer(&dst[0])), &dlen, sp, C.uLong(len(src)), C.int(level))
	if rc != C.Z_OK {
		return nil, fmt.Errorf("zlib: erro %d", int(rc))
	}
	return dst[:int(dlen)], nil
}

const haveZlib = true
