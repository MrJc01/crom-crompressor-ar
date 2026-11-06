package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Experimento (não roda por padrão): CROM_EXP=<pasta de dados> go test -run TestExp
func TestExp(t *testing.T) {
	src := os.Getenv("CROM_EXP")
	if src == "" {
		t.Skip()
	}
	for _, lv := range []zstd.EncoderLevel{zstd.SpeedDefault, zstd.SpeedBetterCompression, zstd.SpeedBestCompression} {
		for _, fs := range []int{1 << 20, 4 << 20} {
			dir := t.TempDir()
			s, _ := Open(dir, Options{Level: lv, FrameSize: fs})
			var files []string
			filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().IsRegular() {
					files = append(files, p)
				}
				return nil
			})
			sort.Strings(files)
			t0 := time.Now()
			var orig int64
			for _, f := range files {
				r, err := s.PutFile(f)
				if err != nil {
					t.Fatal(err)
				}
				orig += r.Size
			}
			el := time.Since(t0)
			st, _ := s.Stats()
			fmt.Printf("%-8s nivel=%-22v frame=%dK disco=%.2fMB  %.1fs  %.1f MB/s\n", filepath.Base(src), lv, fs>>10, float64(st.DiskBytes)/1e6*1e6/1048576, el.Seconds(), float64(orig)/1048576/el.Seconds())
			s.Close()
		}
	}
}
