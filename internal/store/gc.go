package store

import (
	"os"
	"path/filepath"
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
// mortos é ≥ minDead (ex.: 0.3) é reescrito só com os blocos vivos; um sem
// nenhum bloco vivo é apagado. Seguro contra queda: os blocos movidos são
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
	hs, err := s.Contents()
	if err != nil {
		return res, err
	}
	for _, h := range hs {
		r, err := s.Recipe(h)
		if err != nil {
			return res, err // receita ilegível: não arrisca apagar nada
		}
		for _, c := range r.Chunks {
			live[c.Hash] = true
		}
	}

	ids, err := s.segIDs()
	if err != nil {
		return res, err
	}
	liveBytes := map[uint32]int64{}
	allBytes := map[uint32]int64{}
	for h, l := range idx {
		allBytes[l.seg] += int64(l.length)
		if live[h] {
			liveBytes[l.seg] += int64(l.length)
			res.LiveChunks++
		} else {
			res.DeadChunks++
		}
	}
	rewrite := map[uint32]bool{}
	for _, id := range ids {
		if allBytes[id] == 0 || liveBytes[id] == 0 {
			rewrite[id] = true // sem bloco vivo (ou lixo de queda): só apagar
			continue
		}
		if dead := 1 - float64(liveBytes[id])/float64(allBytes[id]); dead >= minDead {
			rewrite[id] = true
		}
	}
	if len(rewrite) == 0 {
		return res, nil
	}

	// copia os blocos vivos dos segmentos reescritos para um segmento novo
	newIdx := map[Hash]loc{}
	var moving []Hash
	var written int64
	for h, l := range idx {
		if !rewrite[l.seg] {
			newIdx[h] = l
		} else if live[h] {
			moving = append(moving, h)
		}
	}
	if len(moving) > 0 {
		w, err := s.openSegWriter(true)
		if err != nil {
			return res, err
		}
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
					newIdx[c.h] = loc{seg: seg, frameOff: off, offInFrame: c.off, length: c.n}
				}
			}
			batch = batch[:0]
			return nil
		}
		for _, h := range moving {
			data, err := s.chunk(h)
			if err != nil {
				w.close()
				return res, err
			}
			fch = append(fch, pendChunk{h: h, off: uint32(len(frame)), n: uint32(len(data))})
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
	hs, err := s.Contents()
	if err != nil {
		return 0, 0, err
	}
	live := map[Hash]bool{}
	for _, h := range hs {
		r, err := s.Recipe(h)
		if err != nil {
			return 0, 0, err
		}
		for _, c := range r.Chunks {
			live[c.Hash] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return 0, 0, err
	}
	for h, l := range s.idx {
		if !live[h] {
			deadChunks++
			deadBytes += int64(l.length)
		}
	}
	return deadChunks, deadBytes, nil
}
