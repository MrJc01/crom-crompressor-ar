package store

import "io"

// Corte por conteúdo (FastCDC com normalização, Xia et al. 2016).
//
// O ponto de corte depende só dos bytes perto dele: inserir ou apagar bytes no
// meio de um arquivo muda só os blocos em volta, e o resto continua igual —
// é isso que faz duas versões de um arquivo dividirem quase todos os blocos.
//
// ATENÇÃO: a tabela gear e as constantes fazem parte do formato. Mudá-las não
// corrompe nada (a receita guarda os hashes), mas faz dados novos pararem de
// deduplicar contra os antigos.
const (
	MinChunk = 4 << 10
	AvgChunk = 16 << 10
	MaxChunk = 64 << 10

	bitsS = 16 // antes da média: corte mais difícil (2^-16)
	bitsL = 12 // depois da média: corte mais fácil (2^-12)
)

var gear [256]uint64

func init() {
	x := uint64(0x9E3779B97F4A7C15) // xorshift64 determinístico
	for i := range gear {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		gear[i] = x
	}
}

// cutPoint devolve o tamanho do primeiro bloco de b.
func cutPoint(b []byte) int {
	n := len(b)
	if n <= MinChunk {
		return n
	}
	if n > MaxChunk {
		n = MaxChunk
	}
	norm := AvgChunk
	if norm > n {
		norm = n
	}
	var h uint64
	i := MinChunk
	for ; i < norm; i++ {
		h = (h << 1) + gear[b[i]]
		if h>>(64-bitsS) == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = (h << 1) + gear[b[i]]
		if h>>(64-bitsL) == 0 {
			return i + 1
		}
	}
	return n
}

// chunker corta um io.Reader em blocos sem carregar o arquivo inteiro.
type chunker struct {
	r          io.Reader
	buf        []byte
	start, end int
	eof        bool
}

func newChunker(r io.Reader) *chunker {
	return &chunker{r: r, buf: make([]byte, 8*MaxChunk)}
}

// next devolve o próximo bloco; o slice só vale até a próxima chamada.
func (c *chunker) next() ([]byte, error) {
	if c.end-c.start < MaxChunk && !c.eof {
		copy(c.buf, c.buf[c.start:c.end])
		c.end -= c.start
		c.start = 0
		for c.end < len(c.buf) && !c.eof {
			n, err := c.r.Read(c.buf[c.end:])
			c.end += n
			if err == io.EOF {
				c.eof = true
			} else if err != nil {
				return nil, err
			}
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := cutPoint(c.buf[c.start:c.end])
	ch := c.buf[c.start : c.start+n]
	c.start += n
	return ch, nil
}
