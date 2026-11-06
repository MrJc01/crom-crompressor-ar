package store

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"fmt"
	"io"
)

// Limites da transformação zlib: acima disso o arquivo vai como está.
const (
	xformMaxOrig  = 32 << 20  // tamanho do arquivo comprimido
	xformMaxPlain = 128 << 20 // tamanho descomprimido (protege contra zip bomb)
)

type zlibPlan struct {
	plain []byte
	level int
	size  int64
	hash  Hash
}

// sniffZlib olha o começo de r. Se o conteúdo inteiro é um stream zlib que a
// zlib do sistema recria byte a byte em algum nível, devolve o conteúdo
// descomprimido e o nível. Senão devolve um reader equivalente ao original.
func sniffZlib(r io.Reader) (io.Reader, *zlibPlan) {
	if !haveZlib {
		return r, nil
	}
	br := bufio.NewReaderSize(r, 64<<10)
	hd, err := br.Peek(2)
	if err != nil || !zlibHeader(hd[0], hd[1]) {
		return br, nil
	}
	flevel := hd[1] >> 6 // hd aponta para o buffer do bufio: ler antes do ReadAll
	buf, err := io.ReadAll(io.LimitReader(br, xformMaxOrig+1))
	rest := io.MultiReader(bytes.NewReader(buf), br)
	if err != nil || len(buf) > xformMaxOrig {
		return rest, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(buf))
	if err != nil {
		return rest, nil
	}
	plain, err := io.ReadAll(io.LimitReader(zr, xformMaxPlain+1))
	if err != nil || len(plain) > xformMaxPlain {
		return rest, nil
	}
	for _, lv := range zlibLevels(flevel) {
		if z, err := zlibCompress(plain, lv); err == nil && bytes.Equal(z, buf) {
			return nil, &zlibPlan{plain: plain, level: lv, size: int64(len(buf)), hash: sha256.Sum256(buf)}
		}
	}
	return rest, nil
}

// zlibHeader: CM=8 (deflate), janela ≤ 32 KB, checksum do cabeçalho, sem dicionário.
func zlibHeader(cmf, flg byte) bool {
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0 && flg&0x20 == 0
}

// zlibLevels: níveis que a zlib marca com cada FLEVEL do cabeçalho.
func zlibLevels(flevel byte) []int {
	switch flevel {
	case 0:
		return []int{1, 0}
	case 1:
		return []int{2, 3, 4, 5}
	case 2:
		return []int{6}
	default:
		return []int{9, 7, 8}
	}
}

// materialize recria o conteúdo original de uma receita com transformação e
// confere o SHA-256 (garante que a recompressão saiu idêntica).
func (f *File) materialize() error {
	f.once.Do(func() {
		var inner bytes.Buffer
		for _, c := range f.r.Chunks {
			data, err := f.s.chunk(c.Hash)
			if err != nil {
				f.perr = err
				return
			}
			if Hash(sha256.Sum256(data)) != c.Hash {
				f.perr = fmt.Errorf("bloco %s corrompido", c.Hash.String()[:12])
				return
			}
			inner.Write(data)
		}
		out, err := zlibCompress(inner.Bytes(), int(f.r.Level))
		if err != nil {
			f.perr = err
			return
		}
		if int64(len(out)) != f.r.Size || Hash(sha256.Sum256(out)) != f.r.Hash {
			f.perr = fmt.Errorf("conteúdo %s: recompressão zlib não bate", f.r.Hash.String()[:12])
			return
		}
		f.plain = out
	})
	return f.perr
}
