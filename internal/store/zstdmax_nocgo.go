//go:build !cgo

package store

func zstdMax(raw []byte) []byte { return nil }

const haveZstdMax = false
