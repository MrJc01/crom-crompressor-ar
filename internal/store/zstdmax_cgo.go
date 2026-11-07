//go:build cgo

package store

import "github.com/DataDog/zstd"

// zstdMax comprime com a libzstd em C no nível 19 (o encoder em Go para no
// ~11). O resultado é zstd comum: o decoder em Go lê igual.
func zstdMax(raw []byte) []byte {
	z, err := zstd.CompressLevel(make([]byte, 0, len(raw)/2), raw, 19)
	if err != nil {
		return nil
	}
	return z
}

const haveZstdMax = true
