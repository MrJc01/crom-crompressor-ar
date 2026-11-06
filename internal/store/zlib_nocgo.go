//go:build !cgo

package store

import "errors"

func zlibCompress(src []byte, level int) ([]byte, error) {
	return nil, errors.New("zlib: binário compilado sem cgo")
}

const haveZlib = false
