package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Experimento (não roda por padrão):
// CROM_EXP_FRAMES=<pasta> [CROM_EXP_FS="1 4 16"] go test -run TestExpFrames -v
// Tamanho do frame × disco × tempo de leitura de arquivos pequenos com cache frio.
func TestExpFrames(t *testing.T) {
	src := os.Getenv("CROM_EXP_FRAMES")
	if src == "" {
		t.Skip()
	}
	sizes := strings.Fields(os.Getenv("CROM_EXP_FS"))
	if len(sizes) == 0 {
		sizes = []string{"1", "4", "16"}
	}
	var files []string
	filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	for _, fsz := range sizes {
		mb, _ := strconv.Atoi(fsz)
		dir := t.TempDir()
		s, err := Open(dir, Options{FrameSize: mb << 20})
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		b, _ := s.Begin()
		var hs []Hash
		var szs []int64
		for _, f := range files {
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			r, err := b.Put(fh)
			fh.Close()
			if err != nil {
				t.Fatal(err)
			}
			hs = append(hs, r.Hash)
			szs = append(szs, r.Size)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		add := time.Since(t0)
		st, _ := s.Stats()
		s.Close()
		// leitura fria: 300 arquivos pequenos ao acaso, depósito reaberto
		s, _ = Open(dir, Options{FrameSize: mb << 20})
		rnd := rand.New(rand.NewSource(1))
		var nread int
		t1 := time.Now()
		for i := 0; i < 3000 && nread < 300; i++ {
			k := rnd.Intn(len(hs))
			if szs[k] > 64<<10 {
				continue
			}
			f, err := s.Open(hs[k])
			if err != nil {
				t.Fatal(err)
			}
			p := make([]byte, szs[k])
			f.ReadAt(p, 0)
			nread++
		}
		rd := time.Since(t1)
		s.Close()
		fmt.Printf("frame %2d MiB: disco %8.1f MB  add %5.1fs  ler %d pequenos %6.0f ms (%.1f ms cada)\n",
			mb, float64(st.DiskBytes)/1e6, add.Seconds(), nread, float64(rd.Milliseconds()), float64(rd.Microseconds())/1000/float64(max(nread, 1)))
	}
}
