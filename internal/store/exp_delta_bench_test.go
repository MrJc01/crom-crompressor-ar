package store

import (
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func deltaPair() ([]byte, []byte) {
	base := make([]byte, 16<<10)
	rand.New(rand.NewSource(3)).Read(base)
	nw := append([]byte(nil), base...)
	for i := 0; i < 8; i++ {
		nw[i*2000+5] ^= 0x55
	}
	return base, nw
}

func BenchmarkDeltaEnc(b *testing.B) {
	base, nw := deltaPair()
	var n int
	for i := 0; i < b.N; i++ {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDictRaw(1, base), zstd.WithLowerEncoderMem(true), zstd.WithWindowSize(1<<17))
		n = len(e.EncodeAll(nw, nil))
		e.Close()
	}
	b.ReportMetric(float64(n), "bytes")
}

func BenchmarkDeltaDec(b *testing.B) {
	base, nw := deltaPair()
	e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDictRaw(1, base))
	z := e.EncodeAll(nw, nil)
	for i := 0; i < b.N; i++ {
		d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDictRaw(1, base), zstd.WithDecoderLowmem(true))
		out, err := d.DecodeAll(z, nil)
		d.Close()
		if err != nil || len(out) != len(nw) {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeltaEncNivel(b *testing.B) {
	base, nw := deltaPair()
	for _, lv := range []zstd.EncoderLevel{zstd.SpeedFastest, zstd.SpeedDefault} {
		for _, win := range []int{1 << 15, 1 << 17} {
			b.Run(lv.String()+"/"+itoa(win>>10)+"K", func(b *testing.B) {
				var n int
				for i := 0; i < b.N; i++ {
					e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(lv), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDictRaw(1, base), zstd.WithLowerEncoderMem(true), zstd.WithWindowSize(win))
					n = len(e.EncodeAll(nw, nil))
					e.Close()
				}
				b.ReportMetric(float64(n), "bytes")
			})
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
