package store

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
)

// Formato em disco (versão 1)
//
//	store/
//	  FORMAT            "crom-ar store v1"
//	  lock              flock entre processos (CLI × residente)
//	  seg/00000001.seg  segmentos append-only: sequência de frames
//	  index.log         registros hash → posição, append-only
//	  recipes/ab/<sha256>.rcp  receita de cada conteúdo (lista de blocos)
//
// Frame:   magic "CRF1" | codec u8 | 3 zeros | rawLen u32 | compLen u32 | dados | crc32c(dados) u32
// Índice:  hash [32] | seg u32 | frameOff u64 | offInFrame u32 | len u32 | crc32c u32   (56 bytes)
// Receita: magic "CRR1" | versão u8 | 3 zeros | size u64 | n u32 | n × (hash [32] | len u32) | crc32c u32

const (
	formatTag    = "crom-ar store v1\n"
	frameMagic   = "CRF1"
	recipeMagic  = "CRR1"
	frameHdrLen  = 16
	frameTailLen = 4
	idxRecLen    = 56

	codecRaw  = 0
	codecZstd = 1
)

var crcTab = crc32.MakeTable(crc32.Castagnoli)

// Hash identifica um bloco ou um conteúdo inteiro (SHA-256).
type Hash [32]byte

func (h Hash) String() string { return fmt.Sprintf("%x", h[:]) }

// ParseHash lê um hash em hexadecimal (64 caracteres).
func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return h, fmt.Errorf("hash inválido: %q", s)
	}
	copy(h[:], b)
	return h, nil
}

// loc é onde um bloco mora.
type loc struct {
	seg        uint32
	frameOff   uint64
	offInFrame uint32
	length     uint32
}

func encodeIdx(h Hash, l loc) []byte {
	b := make([]byte, idxRecLen)
	copy(b, h[:])
	binary.LittleEndian.PutUint32(b[32:], l.seg)
	binary.LittleEndian.PutUint64(b[36:], l.frameOff)
	binary.LittleEndian.PutUint32(b[44:], l.offInFrame)
	binary.LittleEndian.PutUint32(b[48:], l.length)
	binary.LittleEndian.PutUint32(b[52:], crc32.Checksum(b[:52], crcTab))
	return b
}

func decodeIdx(b []byte) (Hash, loc, bool) {
	var h Hash
	if len(b) < idxRecLen || crc32.Checksum(b[:52], crcTab) != binary.LittleEndian.Uint32(b[52:]) {
		return h, loc{}, false
	}
	copy(h[:], b[:32])
	return h, loc{
		seg:        binary.LittleEndian.Uint32(b[32:]),
		frameOff:   binary.LittleEndian.Uint64(b[36:]),
		offInFrame: binary.LittleEndian.Uint32(b[44:]),
		length:     binary.LittleEndian.Uint32(b[48:]),
	}, true
}

func frameHeader(codec byte, rawLen, compLen int) []byte {
	b := make([]byte, frameHdrLen)
	copy(b, frameMagic)
	b[4] = codec
	binary.LittleEndian.PutUint32(b[8:], uint32(rawLen))
	binary.LittleEndian.PutUint32(b[12:], uint32(compLen))
	return b
}

type frameHdr struct {
	codec           byte
	rawLen, compLen uint32
}

func parseFrameHeader(b []byte) (frameHdr, error) {
	if len(b) < frameHdrLen || string(b[:4]) != frameMagic {
		return frameHdr{}, errors.New("frame: cabeçalho inválido")
	}
	return frameHdr{codec: b[4], rawLen: binary.LittleEndian.Uint32(b[8:]), compLen: binary.LittleEndian.Uint32(b[12:])}, nil
}

// Recipe é a lista de blocos que monta um conteúdo.
type Recipe struct {
	Hash   Hash
	Size   int64 // tamanho original
	Chunks []RecipeChunk
	// Xform diz como os blocos viram o conteúdo original. xformZlib: os
	// blocos são o conteúdo descomprimido e a leitura recomprime com zlib no
	// nível Level (versão 2 da receita).
	Xform byte
	Level byte
}

const (
	xformNone = 0
	xformZlib = 1
)

// RecipeChunk é um bloco da receita.
type RecipeChunk struct {
	Hash Hash
	Len  uint32
}

func encodeRecipe(r *Recipe) []byte {
	b := make([]byte, 0, 20+36*len(r.Chunks)+4)
	b = append(b, recipeMagic...)
	if r.Xform == xformNone {
		b = append(b, 1, 0, 0, 0)
	} else {
		b = append(b, 2, r.Xform, r.Level, 0)
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(r.Size))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(r.Chunks)))
	for _, c := range r.Chunks {
		b = append(b, c.Hash[:]...)
		b = binary.LittleEndian.AppendUint32(b, c.Len)
	}
	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, crcTab))
}

func decodeRecipe(h Hash, b []byte) (*Recipe, error) {
	if len(b) < 24 || string(b[:4]) != recipeMagic {
		return nil, errors.New("receita: cabeçalho inválido")
	}
	body := b[:len(b)-4]
	if crc32.Checksum(body, crcTab) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, errors.New("receita: checksum não bate")
	}
	if b[4] != 1 && b[4] != 2 {
		return nil, fmt.Errorf("receita: versão %d desconhecida", b[4])
	}
	r := &Recipe{Hash: h, Size: int64(binary.LittleEndian.Uint64(b[8:]))}
	if b[4] == 2 {
		r.Xform, r.Level = b[5], b[6]
		if r.Xform != xformZlib || r.Level > 9 {
			return nil, fmt.Errorf("receita: transformação %d/%d desconhecida", r.Xform, r.Level)
		}
	}
	n := int(binary.LittleEndian.Uint32(b[16:]))
	if len(body) != 20+36*n {
		return nil, errors.New("receita: tamanho não bate")
	}
	r.Chunks = make([]RecipeChunk, n)
	var total int64
	for i := 0; i < n; i++ {
		p := body[20+36*i:]
		copy(r.Chunks[i].Hash[:], p[:32])
		r.Chunks[i].Len = binary.LittleEndian.Uint32(p[32:])
		total += int64(r.Chunks[i].Len)
	}
	if r.Xform == xformNone && total != r.Size {
		return nil, errors.New("receita: soma dos blocos não bate com o tamanho")
	}
	return r, nil
}
