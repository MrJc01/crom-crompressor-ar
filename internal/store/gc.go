package store

import (
	"os"
	"path/filepath"
	"sort"
)

// GCResult descreve uma coleta.
type GCResult struct {
	LiveChunks        int   `json:"live_chunks"`
	DeadChunks        int   `json:"dead_chunks"`
	SegmentsRewritten int   `json:"segments_rewritten"`
	SegmentsDeleted   int   `json:"segments_deleted"`
	FreedBytes        int64 `json:"freed_bytes"`
}

// GC apaga blocos que nenhuma receita usa. Um segmento cuja fração de bytes
// recuperáveis é ≥ minDead (ex.: 0.3) é reescrito; um sem nenhum bloco vivo é
// apagado. Na reescrita, frames quase todos vivos são copiados sem
// recomprimir (barato); só os muito mortos têm os vivos recomprimidos.
// minDead 0 compacta tudo (doctor --prune). Seguro contra queda: os blocos movidos são
// gravados num segmento novo, o índice novo entra por rename atômico, e só
// então os segmentos antigos são apagados (sobra lixo, nunca falta bloco).
func (s *Store) GC(minDead float64) (GCResult, error) {
	var res GCResult
	s.wmu.Lock()
	defer s.wmu.Unlock()
	lk, err := s.flock()
	if err != nil {
		return res, err
	}
	defer funlock(lk)
	s.mu.Lock()
	err = s.refreshLocked(true)
	idx := make(map[Hash]loc, len(s.idx))
	for h, l := range s.idx {
		idx[h] = l
	}
	s.mu.Unlock()
	if err != nil {
		return res, err
	}

	// marca: blocos usados por alguma receita
	live := map[Hash]bool{}
	rs, err := s.liveRecipes()
	if err != nil {
		return res, err
	}
	for _, r := range rs {
		for _, c := range r.Chunks {
			live[c.Hash] = true
		}
	}
	// bloco em delta precisa da base (que pode não estar em receita nenhuma)
	if err := s.markDeltaBases(live); err != nil {
		return res, err
	}
	if err := s.compactRecipesLocked(rs); err != nil {
		return res, err
	}

	ids, err := s.segIDs()
	if err != nil {
		return res, err
	}
	// contabilidade por frame: o que dá para recuperar sem reescrever tudo
	type fkey struct {
		seg uint32
		off uint64
	}
	type fstat struct{ all, live int64 }
	frames := map[fkey]*fstat{}
	for h, l := range idx {
		k := fkey{l.seg, l.frameOff}
		f := frames[k]
		if f == nil {
			f = &fstat{}
			frames[k] = f
		}
		f.all += int64(l.size())
		if live[h] {
			f.live += int64(l.size())
			res.LiveChunks++
		} else {
			res.DeadChunks++
		}
	}
	// Frame sem bloco vivo: sai. Frame com menos de minDead morto: copiado
	// como está (sem descomprimir nem recomprimir; os blocos mortos dentro
	// dele continuam no índice e podem ser reaproveitados). Os demais: os
	// blocos vivos são recomprimidos num frame novo.
	verbatim := func(f *fstat) bool {
		return f.live > 0 && float64(f.all-f.live) < minDead*float64(f.all)
	}
	reclaim := map[uint32]int64{}
	allBytes := map[uint32]int64{}
	liveBytes := map[uint32]int64{}
	for k, f := range frames {
		allBytes[k.seg] += f.all
		liveBytes[k.seg] += f.live
		if !verbatim(f) {
			reclaim[k.seg] += f.all - f.live
		}
	}
	rewrite := map[uint32]bool{}
	for _, id := range ids {
		if allBytes[id] == 0 || liveBytes[id] == 0 {
			rewrite[id] = true // sem bloco vivo (ou lixo de queda): só apagar
			continue
		}
		if r := float64(reclaim[id]) / float64(allBytes[id]); r > 0 && r >= minDead {
			rewrite[id] = true
		}
	}
	if len(rewrite) == 0 {
		return res, nil
	}

	newIdx := map[Hash]loc{}
	var copyFrames []fkey
	var moving []Hash
	var written int64
	for h, l := range idx {
		k := fkey{l.seg, l.frameOff}
		switch {
		case !rewrite[l.seg]:
			newIdx[h] = l
		case verbatim(frames[k]):
			// mapeado depois da cópia do frame
		case live[h]:
			moving = append(moving, h)
		}
	}
	for k, f := range frames {
		if rewrite[k.seg] && verbatim(f) {
			copyFrames = append(copyFrames, k)
		}
	}
	sort.Slice(copyFrames, func(i, j int) bool {
		if copyFrames[i].seg != copyFrames[j].seg {
			return copyFrames[i].seg < copyFrames[j].seg
		}
		return copyFrames[i].off < copyFrames[j].off
	})
	if len(moving) > 0 || len(copyFrames) > 0 {
		w, err := s.openSegWriter(true)
		if err != nil {
			return res, err
		}
		// 1) frames copiados byte a byte
		byFrame := map[fkey][]Hash{}
		for h, l := range idx {
			k := fkey{l.seg, l.frameOff}
			if rewrite[l.seg] && verbatim(frames[k]) {
				byFrame[k] = append(byFrame[k], h)
			}
		}
		for _, k := range copyFrames {
			hd, comp, err := s.readRawFrame(k.seg, k.off)
			if err != nil {
				w.close()
				return res, err
			}
			seg, off, err := w.writeFrame(hd.codec, int(hd.rawLen), comp)
			if err != nil {
				w.close()
				return res, err
			}
			written += int64(frameHdrLen + len(comp) + frameTailLen)
			for _, h := range byFrame[k] {
				l := idx[h]
				newIdx[h] = loc{seg: seg, frameOff: off, offInFrame: l.offInFrame, length: l.length}
			}
		}
		// 2) blocos vivos de frames muito mortos: recomprimidos juntos
		var batch []*frameJob
		frame := make([]byte, 0, s.opt.FrameSize+MaxChunk)
		var fch []pendChunk
		flush := func() error {
			s.compressBatch(batch)
			for _, j := range batch {
				seg, off, err := w.writeFrame(j.codec, len(j.raw), j.data)
				if err != nil {
					return err
				}
				written += int64(frameHdrLen + len(j.data) + frameTailLen)
				for _, c := range j.chunks {
					newIdx[c.h] = loc{seg: seg, frameOff: off, offInFrame: c.off, length: c.length()}
				}
			}
			batch = batch[:0]
			return nil
		}
		// em ordem de posição: lê cada frame antigo uma vez só (cache)
		sort.Slice(moving, func(i, j int) bool {
			a, b := idx[moving[i]], idx[moving[j]]
			if a.seg != b.seg {
				return a.seg < b.seg
			}
			if a.frameOff != b.frameOff {
				return a.frameOff < b.frameOff
			}
			return a.offInFrame < b.offInFrame
		})
		for _, h := range moving {
			// delta vai como está (a base também fica viva)
			data, l, err := s.chunkPayload(h)
			if err != nil {
				w.close()
				return res, err
			}
			fch = append(fch, pendChunk{h: h, off: uint32(len(frame)), n: uint32(len(data)), delta: l.delta()})
			frame = append(frame, data...)
			if len(frame) >= s.opt.FrameSize {
				batch = append(batch, &frameJob{raw: frame, chunks: fch})
				frame, fch = make([]byte, 0, s.opt.FrameSize+MaxChunk), nil
				if len(batch) >= s.opt.Workers {
					if err := flush(); err != nil {
						w.close()
						return res, err
					}
				}
			}
		}
		if len(frame) > 0 {
			batch = append(batch, &frameJob{raw: frame, chunks: fch})
		}
		if err := flush(); err != nil {
			w.close()
			return res, err
		}
		if err := w.close(); err != nil {
			return res, err
		}
	}

	// índice novo por rename atômico
	buf := make([]byte, 0, len(newIdx)*idxRecLen)
	for h, l := range newIdx {
		buf = append(buf, encodeIdx(h, l)...)
	}
	if err := writeFileSync(s.idxPath(), buf); err != nil {
		return res, err
	}
	s.dropSegReaders()
	s.cache.clear()
	for id := range rewrite {
		if fi, err := os.Stat(s.segPath(id)); err == nil {
			res.FreedBytes += fi.Size()
		}
		if err := os.Remove(s.segPath(id)); err == nil {
			if liveBytes[id] > 0 {
				res.SegmentsRewritten++
			} else {
				res.SegmentsDeleted++
			}
		}
	}
	res.FreedBytes -= written
	syncDir(filepath.Join(s.dir, "seg"))
	s.mu.Lock()
	s.idxIno = 0
	err = s.refreshLocked(true)
	s.mu.Unlock()
	return res, err
}

// Garbage conta, sem mudar nada, os blocos que nenhuma receita usa.
func (s *Store) Garbage() (deadChunks int, deadBytes int64, err error) {
	rs, err := s.liveRecipes()
	if err != nil {
		return 0, 0, err
	}
	live := map[Hash]bool{}
	for _, r := range rs {
		for _, c := range r.Chunks {
			live[c.Hash] = true
		}
	}
	if err := s.markDeltaBases(live); err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return 0, 0, err
	}
	for h, l := range s.idx {
		if !live[h] {
			deadChunks++
			deadBytes += int64(l.size())
		}
	}
	return deadChunks, deadBytes, nil
}
