package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// Blocos parecidos (item 38). Arquivos quase iguais (o mesmo áudio exportado
// de novo, uma versão nova de um binário) têm diferenças espalhadas: quase
// todo bloco muda em poucos bytes e a deduplicação exata não pega nada. Cada
// bloco novo procura um bloco já guardado com a mesma "super-feature"
// (minhash do conteúdo, como no Finesse/odess); se achar, guarda só a
// diferença — zstd com o bloco base como dicionário.
//
// No frame, um bloco em delta é: hash do bloco base [32] | zstd. No índice, o
// bit alto do tamanho marca o delta. A base nunca é um delta (sem cadeias).
// feats.log guarda as features dos blocos que podem servir de base:
//
//	hash [32] | f0 u64 | f1 u64 | f2 u64 | crc32c u32   (60 bytes)

const (
	deltaFlag    = 1 << 31
	featRecLen   = 60
	deltaHashLen = 32
	// bloco menor que isso não procura parecido: ganharia poucos bytes e a
	// feature de bloco minúsculo é fraca (todos os de < 64 bytes empatam)
	deltaMinChunk = 1 << 10
)

func (l loc) size() uint32 { return l.length &^ deltaFlag }
func (l loc) delta() bool  { return l.length&deltaFlag != 0 }

var featMul = [3]uint64{0x9E3779B97F4A7C15, 0xC2B2AE3D27D4EB4F, 0x165667B19E3779F9}

// features: para 3 transformações do gear hash rolante (janela de 64 bytes),
// o mínimo sobre o bloco. Blocos que diferem em poucos bytes quase sempre
// têm ao menos uma feature igual.
func features(b []byte) [3]uint64 {
	m := [3]uint64{^uint64(0), ^uint64(0), ^uint64(0)}
	var h uint64
	for i, c := range b {
		h = (h << 1) + gear[c]
		if i < 63 {
			continue
		}
		for k := range m {
			x := h * featMul[k]
			x ^= x >> 31
			if x < m[k] {
				m[k] = x
			}
		}
	}
	return m
}

// deltaLevel: criar o encoder com o dicionário é o que custa (0,16 ms no
// rápido, 0,8 ms no padrão por tentativa); a diferença de tamanho é pequena.
var deltaLevel = zstd.SpeedFastest

func encodeDelta(base, data []byte) []byte {
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(deltaLevel), zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDictRaw(1, base), zstd.WithLowerEncoderMem(true), zstd.WithWindowSize(1<<17))
	if err != nil {
		return nil
	}
	defer e.Close()
	return e.EncodeAll(data, nil)
}

func decodeDelta(base, z []byte) ([]byte, error) {
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDictRaw(1, base),
		zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(1<<20))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.DecodeAll(z, nil)
}

// undelta recria um bloco guardado como diferença.
func (s *Store) undelta(payload []byte, depth int) ([]byte, error) {
	if len(payload) < deltaHashLen || depth > 0 {
		return nil, errors.New("bloco em delta inválido")
	}
	var hb Hash
	copy(hb[:], payload[:deltaHashLen])
	base, err := s.chunkDepth(hb, depth+1)
	if err != nil {
		return nil, fmt.Errorf("base do delta: %w", err)
	}
	return decodeDelta(base, payload[deltaHashLen:])
}

// deltaBase devolve o hash do bloco base de um bloco em delta.
func (s *Store) deltaBase(h Hash) (Hash, error) {
	var hb Hash
	p, _, err := s.chunkPayload(h)
	if err != nil {
		return hb, err
	}
	if len(p) < deltaHashLen {
		return hb, errors.New("bloco em delta inválido")
	}
	copy(hb[:], p[:deltaHashLen])
	return hb, nil
}

// markDeltaBases acrescenta a live as bases de todos os blocos em delta do
// índice, não só dos vivos: um delta morto pode continuar no índice (frame
// copiado como está) e a deduplicação o reaproveitaria sem a base.
func (s *Store) markDeltaBases(live map[Hash]bool) error {
	var deltas []Hash
	s.mu.RLock()
	for h, l := range s.idx {
		if l.delta() {
			deltas = append(deltas, h)
		}
	}
	s.mu.RUnlock()
	for _, h := range deltas {
		hb, err := s.deltaBase(h)
		if err != nil {
			return err
		}
		live[hb] = true
	}
	return nil
}

// ---------------------------------------------------------------- feats.log

func (s *Store) featPath() string { return filepath.Join(s.dir, "feats.log") }

func encodeFeat(h Hash, f [3]uint64) []byte {
	b := make([]byte, featRecLen)
	copy(b, h[:])
	for k, v := range f {
		binary.LittleEndian.PutUint64(b[32+8*k:], v)
	}
	binary.LittleEndian.PutUint32(b[56:], crc32.Checksum(b[:56], crcTab))
	return b
}

// addFeat registra h como possível base (só na memória do escritor).
func (s *Store) addFeat(h Hash, f [3]uint64) {
	n := uint32(len(s.featH))
	s.featH = append(s.featH, h)
	for _, v := range f {
		if _, ok := s.featM[v]; !ok {
			s.featM[v] = n
		}
	}
}

// findBase devolve um bloco parecido com features f (diferente de h).
func (s *Store) findBase(h Hash, f [3]uint64) (Hash, bool) {
	for _, v := range f {
		if i, ok := s.featM[v]; ok && s.featH[i] != h {
			return s.featH[i], true
		}
	}
	return Hash{}, false
}

// loadFeatsLocked lê o que falta do feats.log (com o flock de escritor).
func (s *Store) loadFeatsLocked() error {
	if s.featM == nil {
		s.featM = map[uint64]uint32{}
	}
	f, err := os.Open(s.featPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() <= s.featOff {
		return nil
	}
	buf := make([]byte, st.Size()-s.featOff)
	if _, err := f.ReadAt(buf, s.featOff); err != nil && err != io.EOF {
		return err
	}
	good := 0
	for good+featRecLen <= len(buf) {
		b := buf[good : good+featRecLen]
		if crc32.Checksum(b[:56], crcTab) != binary.LittleEndian.Uint32(b[56:]) {
			break
		}
		var h Hash
		copy(h[:], b[:32])
		var fv [3]uint64
		for k := range fv {
			fv[k] = binary.LittleEndian.Uint64(b[32+8*k:])
		}
		s.addFeat(h, fv)
		good += featRecLen
	}
	if good < len(buf) {
		if err := os.Truncate(s.featPath(), s.featOff+int64(good)); err != nil {
			return err
		}
	}
	s.featOff += int64(good)
	return nil
}

func (s *Store) appendFeats(recs []byte) error {
	f, err := os.OpenFile(s.featPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(recs); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	s.featOff += int64(len(recs))
	return f.Close()
}

// ---------------------------------------------------------------- no lote

// tryDelta: se c tem um bloco parecido, devolve o payload do delta.
func (b *Batch) tryDelta(h Hash, c []byte, f [3]uint64) []byte {
	s := b.s
	hb, ok := s.findBase(h, f)
	if !ok {
		return nil
	}
	base := b.baseBytes(hb)
	if base == nil {
		return nil
	}
	z := encodeDelta(base, c)
	if z == nil || deltaHashLen+len(z) >= len(c)/2 {
		return nil
	}
	if back, err := decodeDelta(base, z); err != nil || !bytes.Equal(back, c) {
		return nil
	}
	return append(append(make([]byte, 0, deltaHashLen+len(z)), hb[:]...), z...)
}

// baseBytes acha os bytes de um bloco base: ainda na memória do lote, já
// gravado no segmento mas fora do índice, ou no depósito.
func (b *Batch) baseBytes(h Hash) []byte {
	if d, ok := b.mem[h]; ok {
		return d
	}
	if l, ok := b.ploc[h]; ok {
		fr, err := b.s.readFrame(l.seg, l.frameOff)
		if err != nil || int(l.offInFrame)+int(l.size()) > len(fr) {
			return nil
		}
		return fr[l.offInFrame : l.offInFrame+l.size()]
	}
	if l, ok := b.s.lookup(h); ok && !l.delta() {
		d, err := b.s.chunk(h)
		if err == nil {
			return d
		}
	}
	return nil
}
