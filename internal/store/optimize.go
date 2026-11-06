package store

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"sort"
)

// Arrumação dos deltas (itens 52 e 59 do checklist).
//
//   - Base órfã: o bloco A só continua no disco porque o delta de B depende
//     dele (nenhum arquivo usa A). rebaseOrphans troca a base de B por outro
//     bloco vivo parecido, ou guarda B inteiro, quando isso ocupa menos que
//     manter A. Roda no começo de todo GC.
//   - Blocos antigos contra os novos: Optimize lê os blocos vivos guardados
//     inteiros que ainda não têm features (gravados antes dos deltas, ou com
//     NoDelta) e os transforma em delta de um bloco parecido quando compensa.
//   - feats.log: compactFeatsLocked tira as features de blocos que saíram.

// OptimizeResult descreve uma arrumação.
type OptimizeResult struct {
	OrphanBases int   `json:"orphan_bases"` // bases sem arquivo que deixaram de ser necessárias
	Rebased     int   `json:"rebased"`      // deltas que trocaram de base
	Undeltaed   int   `json:"undeltaed"`    // deltas guardados inteiros de novo
	Scanned     int   `json:"scanned"`      // blocos inteiros analisados
	NewDeltas   int   `json:"new_deltas"`   // blocos inteiros que viraram delta
	SavedRaw    int64 `json:"saved_raw"`    // bytes (antes da compressão do frame) economizados
}

// deltaPayload: delta de data contra base, se ficar bem menor que data.
func deltaPayload(hb Hash, base, data []byte) []byte {
	z := encodeDelta(base, data)
	if z == nil || deltaHashLen+len(z) >= len(data)/2 {
		return nil
	}
	if back, err := decodeDelta(base, z); err != nil || !bytes.Equal(back, data) {
		return nil
	}
	return append(append(make([]byte, 0, deltaHashLen+len(z)), hb[:]...), z...)
}

// putPayload grava de novo um bloco que já existe com outra forma (delta ou
// inteiro). O registro novo do índice vence o antigo; o antigo vira lixo.
func (b *Batch) putPayload(h Hash, data []byte, delta bool) error {
	b.fchunks = append(b.fchunks, pendChunk{h: h, off: uint32(len(b.frame)), n: uint32(len(data)), delta: delta})
	b.frame = append(b.frame, data...)
	b.dirty += int64(len(data))
	if len(b.frame) >= b.s.opt.FrameSize {
		return b.cutFrame()
	}
	return nil
}

// register guarda as features de um bloco que foi gravado: se ficou inteiro,
// ele é a base; se virou delta, aponta para a base dele (um bloco novo
// parecido com o delta acha a mesma base).
func (b *Batch) register(base Hash, f [3]uint64) {
	b.s.addFeat(base, f)
	b.feats = append(b.feats, encodeFeat(base, f)...)
}

// recipeChunks: blocos usados por alguma receita.
func (s *Store) recipeChunks() (map[Hash]bool, error) {
	rs, err := s.liveRecipes()
	if err != nil {
		return nil, err
	}
	live := map[Hash]bool{}
	for _, r := range rs {
		for _, c := range r.Chunks {
			live[c.Hash] = true
		}
	}
	return live, nil
}

type hloc struct {
	h Hash
	l loc
}

// byPos devolve os blocos do índice que passam em keep, em ordem de posição.
func (s *Store) byPos(keep func(Hash, loc) bool) []hloc {
	var out []hloc
	s.mu.RLock()
	for h, l := range s.idx {
		if keep(h, l) {
			out = append(out, hloc{h, l})
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].l, out[j].l
		if a.seg != b.seg {
			return a.seg < b.seg
		}
		if a.frameOff != b.frameOff {
			return a.frameOff < b.frameOff
		}
		return a.offInFrame < b.offInFrame
	})
	return out
}

// usableBase: candidato a base aceito numa arrumação.
func (s *Store) usableBase(c, self Hash, live, avoid map[Hash]bool) bool {
	if c == self || !live[c] || avoid[c] {
		return false
	}
	l, ok := s.lookup(c)
	return ok && !l.delta()
}

func (s *Store) pickBase(self Hash, f [3]uint64, live, avoid map[Hash]bool) (Hash, bool) {
	for _, v := range f {
		if i, ok := s.featM[v]; ok && s.usableBase(s.featH[i], self, live, avoid) {
			return s.featH[i], true
		}
	}
	return Hash{}, false
}

// rebaseOrphans resolve os deltas vivos cuja base nenhum arquivo usa.
func (s *Store) rebaseOrphans() (OptimizeResult, error) {
	var res OptimizeResult
	b, err := s.Begin()
	if err != nil {
		return res, err
	}
	defer b.Abort()
	live, err := s.recipeChunks()
	if err != nil {
		return res, err
	}
	deltas := s.byPos(func(h Hash, l loc) bool { return l.delta() && live[h] })
	orphans := map[Hash][]Hash{}
	var order []Hash
	for _, d := range deltas {
		hb, err := s.deltaBase(d.h)
		if err != nil {
			return res, err
		}
		if !live[hb] {
			if orphans[hb] == nil {
				order = append(order, hb)
			}
			orphans[hb] = append(orphans[hb], d.h)
		}
	}
	if len(order) == 0 {
		return res, nil
	}
	avoid := map[Hash]bool{}
	for hb := range orphans {
		avoid[hb] = true
	}
	type plan struct {
		h     Hash
		data  []byte
		delta bool
		base  Hash
		f     [3]uint64
	}
	for _, hb := range order {
		base, err := s.chunk(hb)
		if err != nil {
			return res, err
		}
		keep := int64(len(base))
		var change int64
		var ps []plan
		for _, d := range orphans[hb] {
			p, l, err := s.chunkPayload(d)
			if err != nil {
				return res, err
			}
			keep += int64(l.size())
			full, err := decodeDelta(base, p[deltaHashLen:])
			if err != nil {
				return res, err
			}
			f := features(full)
			pl := plan{h: d, data: full, base: d, f: f}
			if c, ok := s.pickBase(d, f, live, avoid); ok {
				if cb, err := s.chunk(c); err == nil {
					if z := deltaPayload(c, cb, full); z != nil {
						pl.data, pl.delta, pl.base = z, true, c
					}
				}
			}
			change += int64(len(pl.data))
			ps = append(ps, pl)
		}
		if change >= keep {
			continue // manter a base sai mais barato (ela sustenta vários deltas)
		}
		for _, pl := range ps {
			if err := b.putPayload(pl.h, pl.data, pl.delta); err != nil {
				return res, err
			}
			b.register(pl.base, pl.f)
			if pl.delta {
				res.Rebased++
			} else {
				res.Undeltaed++
			}
		}
		res.OrphanBases++
		res.SavedRaw += keep - change
	}
	return res, b.Close()
}

// Optimize procura, entre os blocos vivos guardados inteiros que ainda não
// têm features, os que podem virar delta de um bloco parecido. Cada bloco é
// analisado uma vez: depois disso as features dele ficam no feats.log.
func (s *Store) Optimize() (OptimizeResult, error) {
	var res OptimizeResult
	if s.opt.NoDelta {
		return res, nil
	}
	b, err := s.Begin()
	if err != nil {
		return res, err
	}
	defer b.Abort()
	live, err := s.recipeChunks()
	if err != nil {
		return res, err
	}
	recs, err := s.readFeats()
	if err != nil {
		return res, err
	}
	has := make(map[Hash]bool, len(recs))
	for _, r := range recs {
		has[r.h] = true
	}
	// quem é base de um delta não pode virar delta (sem cadeias)
	isBase := map[Hash]bool{}
	for _, d := range s.byPos(func(_ Hash, l loc) bool { return l.delta() }) {
		hb, err := s.deltaBase(d.h)
		if err != nil {
			return res, err
		}
		isBase[hb] = true
	}
	// bloco que é o arquivo inteiro com receita curta: o tamanho do arquivo
	// vem do índice, então ele não pode virar delta
	short := func(h Hash) bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		l, ok := s.rcp[h]
		return ok && l.off < 0
	}
	todo := s.byPos(func(h Hash, l loc) bool {
		return live[h] && !l.delta() && !has[h] && l.size() >= deltaMinChunk
	})
	converted := map[Hash]bool{}
	for _, c := range todo {
		data, err := s.chunk(c.h)
		if err != nil {
			return res, err
		}
		res.Scanned++
		f := features(data)
		if !isBase[c.h] && !short(c.h) {
			if hb, ok := s.pickBase(c.h, f, live, converted); ok {
				if base, err := s.chunk(hb); err == nil {
					if z := deltaPayload(hb, base, data); z != nil {
						if err := b.putPayload(c.h, z, true); err != nil {
							return res, err
						}
						b.register(hb, f)
						isBase[hb] = true
						converted[c.h] = true
						res.NewDeltas++
						res.SavedRaw += int64(len(data) - len(z))
						continue
					}
				}
			}
		}
		b.register(c.h, f)
	}
	return res, b.Close()
}

// ---------------------------------------------------------------- feats.log

type featRec struct {
	h Hash
	f [3]uint64
}

func (s *Store) readFeats() ([]featRec, error) {
	buf, err := os.ReadFile(s.featPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []featRec
	for i := 0; i+featRecLen <= len(buf); i += featRecLen {
		b := buf[i : i+featRecLen]
		if crc32.Checksum(b[:56], crcTab) != binary.LittleEndian.Uint32(b[56:]) {
			break
		}
		var r featRec
		copy(r.h[:], b[:32])
		for k := range r.f {
			r.f[k] = binary.LittleEndian.Uint64(b[32+8*k:])
		}
		out = append(out, r)
	}
	return out, nil
}

// compactFeatsLocked reescreve o feats.log só com features de blocos que
// ainda podem ser base (no índice e inteiros), sem repetidos. Só reescreve
// quando sobra pelo menos um quarto de lixo (ou force). Chamar com wmu+flock.
func (s *Store) compactFeatsLocked(force bool) error {
	recs, err := s.readFeats()
	if err != nil || len(recs) == 0 {
		return err
	}
	seen := make(map[featRec]bool, len(recs))
	var buf []byte
	s.mu.RLock()
	for _, r := range recs {
		l, ok := s.idx[r.h]
		if !ok || l.delta() || seen[r] {
			continue
		}
		seen[r] = true
		buf = append(buf, encodeFeat(r.h, r.f)...)
	}
	s.mu.RUnlock()
	kept := len(buf) / featRecLen
	if kept == len(recs) || (!force && (len(recs)-kept)*4 < len(recs)) {
		return nil
	}
	if err := writeFileSync(s.featPath(), buf); err != nil {
		return err
	}
	s.featM, s.featH, s.featOff = nil, nil, 0
	return nil
}
