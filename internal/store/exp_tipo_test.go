package store

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"
)

// Experimento (não roda por padrão): CROM_EXP_TIPO=<pasta> go test -run TestExpTipo -v
// Compara: frames na ordem de chegada × frames por tipo × frames por tipo com dicionário treinado.
func TestExpTipo(t *testing.T) {
	src := os.Getenv("CROM_EXP_TIPO")
	if src == "" {
		t.Skip()
	}
	type blk struct {
		class string
		data  []byte
	}
	var files []string
	filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	seen := map[[32]byte]bool{}
	var blocks []blk
	var orig, uniq int64
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		cl := classOf(f)
		c := newChunker(fh)
		for {
			ch, err := c.next()
			if err != nil {
				break
			}
			orig += int64(len(ch))
			h := sha256.Sum256(ch)
			if seen[h] {
				continue
			}
			seen[h] = true
			uniq += int64(len(ch))
			blocks = append(blocks, blk{cl, append([]byte(nil), ch...)})
		}
		fh.Close()
	}
	const frame = 1 << 20
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	pack := func(stream [][]byte, e *zstd.Encoder) int64 {
		var frames [][]byte
		var cur []byte
		for _, b := range stream {
			cur = append(cur, b...)
			if len(cur) >= frame {
				frames = append(frames, cur)
				cur = nil
			}
		}
		if len(cur) > 0 {
			frames = append(frames, cur)
		}
		var tot int64
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, fr := range frames {
			wg.Add(1)
			sem <- struct{}{}
			go func(fr []byte) {
				defer wg.Done()
				n := len(fr)
				if z := e.EncodeAll(fr, nil); len(z) < n*98/100 {
					n = len(z)
				}
				mu.Lock()
				tot += int64(n + frameHdrLen + frameTailLen)
				mu.Unlock()
				<-sem
			}(fr)
		}
		wg.Wait()
		return tot
	}
	// A: ordem de chegada
	var all [][]byte
	byClass := map[string][][]byte{}
	clSize := map[string]int64{}
	for _, b := range blocks {
		all = append(all, b.data)
		byClass[b.class] = append(byClass[b.class], b.data)
		clSize[b.class] += int64(len(b.data))
	}
	a := pack(all, enc)
	// B: por tipo; C: por tipo com dicionário
	var b, c, dictTot int64
	var classes []string
	for k := range byClass {
		classes = append(classes, k)
	}
	sort.Slice(classes, func(i, j int) bool { return clSize[classes[i]] > clSize[classes[j]] })
	for _, k := range classes {
		bk := pack(byClass[k], enc)
		b += bk
		ck := bk
		if clSize[k] >= 1<<20 {
			// amostras: até 8 MiB de blocos espalhados pela classe
			var samples [][]byte
			st := 1
			if clSize[k] > 8<<20 {
				st = int(clSize[k] / (8 << 20))
			}
			for i := 0; i < len(byClass[k]); i += st {
				samples = append(samples, byClass[k][i])
			}
			if d, err := dict.BuildZstdDict(samples, dict.Options{MaxDictSize: 112 << 10, HashBytes: 6, ZstdDictID: 1}); err == nil {
				de, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDict(d))
				if err == nil {
					if dk := pack(byClass[k], de) + int64(len(d)); dk < ck {
						ck = dk
						dictTot += int64(len(d))
					}
					de.Close()
				}
			}
		}
		c += ck
		if clSize[k] > 5<<20 {
			fmt.Printf("  %-8s %8.1f MB  ordem-tipo %8.1f MB  com-dic %8.1f MB\n", k, float64(clSize[k])/1e6, float64(bk)/1e6, float64(ck)/1e6)
		}
	}
	// D: blocos parecidos juntos — ordena por "super-features" (minhash de
	// janelas de 32 bytes), como no Finesse/odess: blocos com a mesma feature
	// têm grande chance de serem quase iguais.
	type sb struct {
		k    [3]uint64
		data []byte
	}
	var sim []sb
	for _, b := range blocks {
		sim = append(sim, sb{minFeatures(b.data), b.data})
	}
	sort.SliceStable(sim, func(i, j int) bool {
		a, b := sim[i].k, sim[j].k
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		if a[1] != b[1] {
			return a[1] < b[1]
		}
		return a[2] < b[2]
	})
	var simStream [][]byte
	for _, x := range sim {
		simStream = append(simStream, x.data)
	}
	d := pack(simStream, enc)
	M := 1e6
	fmt.Printf("%s: original %.1f MB, únicos %.1f MB, %d tipos\n", filepath.Base(src), float64(orig)/M, float64(uniq)/M, len(classes))
	fmt.Printf("A ordem de chegada      %.1f MB\n", float64(a)/M)
	fmt.Printf("B agrupado por tipo     %.1f MB\n", float64(b)/M)
	fmt.Printf("C por tipo + dicionário %.1f MB (dicionários %.1f KB)\n", float64(c)/M, float64(dictTot)/1e3)
	fmt.Printf("D blocos parecidos juntos %.1f MB\n", float64(d)/M)
}

func classOf(p string) string {
	e := strings.ToLower(filepath.Ext(p))
	if e == "" || len(e) > 6 || strings.ContainsAny(e[1:], "0123456789") && len(e) > 3 {
		if strings.Contains(p, "/.git/objects/") {
			return "gitobj"
		}
		f, err := os.Open(p)
		if err != nil {
			return "noext"
		}
		defer f.Close()
		h := make([]byte, 512)
		n, _ := io.ReadFull(f, h)
		for _, x := range h[:n] {
			if x == 0 {
				return "bin"
			}
		}
		return "txt"
	}
	return e
}

// minFeatures: 3 minhashes (sementes diferentes) do hash rolante de janelas de 32 bytes.
func minFeatures(b []byte) [3]uint64 {
	seeds := [3]uint64{0x9E3779B97F4A7C15, 0xC2B2AE3D27D4EB4F, 0x165667B19E3779F9}
	m := [3]uint64{^uint64(0), ^uint64(0), ^uint64(0)}
	const w = 32
	if len(b) < w {
		return m
	}
	var h uint64
	var pow uint64 = 1
	const base = 1099511628211
	for i := 0; i < w; i++ {
		h = h*base + uint64(b[i])
		if i > 0 {
			pow *= base
		}
	}
	for i := w; ; i++ {
		for k, sd := range seeds {
			x := (h ^ sd) * 0xff51afd7ed558ccd
			x ^= x >> 33
			if x < m[k] {
				m[k] = x
			}
		}
		if i >= len(b) {
			break
		}
		h = (h-uint64(b[i-w])*pow)*base + uint64(b[i])
	}
	return m
}
