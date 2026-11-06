package store

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Onde o depósito gasta disco: arquivos de controle, frames crus e comprimidos,
// e quanto os crus ainda comprimiriam. CROM_EXP_GAP=<pasta>.
func TestExpGap(t *testing.T) {
	src := os.Getenv("CROM_EXP_GAP")
	if src == "" {
		t.Skip()
	}
	var files []string
	filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Begin()
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		if _, err := b.Put(fh); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	s.Close()
	M := 1e6
	fmt.Printf("DiskBytes %.1f MB\n", float64(st.DiskBytes)/M)
	byExt := map[string][2]int64{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			e := filepath.Ext(p)
			v := byExt[e]
			v[0] += fi.Size()
			v[1] += allocated(fi)
			byExt[e] = v
		}
		return nil
	})
	for e, v := range byExt {
		fmt.Printf("  %-8s aparente %.1f MB  alocado %.1f MB\n", e, float64(v[0])/M, float64(v[1])/M)
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	var nRaw, nZ, small int
	var rawB, rawZ, zRaw, zComp, gaps int64
	segs, _ := filepath.Glob(filepath.Join(dir, "**", "*.seg"))
	if len(segs) == 0 {
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if filepath.Ext(p) == ".seg" {
				segs = append(segs, p)
			}
			return nil
		})
	}
	for _, sp := range segs {
		data, _ := os.ReadFile(sp)
		off := 0
		for off+frameHdrLen <= len(data) {
			if string(data[off:off+4]) != frameMagic {
				// procura o próximo frame
				gaps++
				off++
				continue
			}
			codec := data[off+4]
			rl := int(binary.LittleEndian.Uint32(data[off+8:]))
			cl := int(binary.LittleEndian.Uint32(data[off+12:]))
			body := data[off+frameHdrLen : off+frameHdrLen+cl]
			if rl < 512<<10 {
				small++
			}
			if codec == codecRaw {
				nRaw++
				rawB += int64(rl)
				rawZ += int64(len(enc.EncodeAll(body, nil)))
			} else {
				nZ++
				zRaw += int64(rl)
				zComp += int64(cl)
			}
			off += frameHdrLen + cl + frameTailLen
		}
	}
	_ = io.EOF
	fmt.Printf("frames crus %d: %.1f MB (zstd-11 daria %.1f MB)\n", nRaw, float64(rawB)/M, float64(rawZ)/M)
	fmt.Printf("frames zstd %d: %.1f → %.1f MB\n", nZ, float64(zRaw)/M, float64(zComp)/M)
	fmt.Printf("frames < 512 KB: %d; bytes fora de frame: %d\n", small, gaps)
}
