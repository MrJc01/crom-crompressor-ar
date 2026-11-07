package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Frames frios (item 62). O add comprime cada frame no nível que cabe no
// tempo dele (zstd ~11). Depois de fechado, um frame nunca muda: dá para
// recomprimi-lo uma vez, em segundo plano, no zstd 19 — uns 10% a menos nos
// dados comprimíveis, e a leitura continua igual (o zstd lê na mesma
// velocidade em qualquer nível). Ler nunca descompacta nada no disco.
//
// codecZstdMax marca o frame já recomprimido (ou que não ganhou nada no 19):
// é zstd comum, só não é tentado de novo.

// ColdResult descreve uma rodada de recompressão.
type ColdResult struct {
	Frames    int   `json:"frames"`    // frames recomprimidos
	Segments  int   `json:"segments"`  // segmentos reescritos
	Before    int64 `json:"before"`    // bytes desses segmentos antes
	After     int64 `json:"after"`     // depois
	Remaining int   `json:"remaining"` // segmentos que ainda têm frame para recomprimir
}

// coldTodo: segmentos com algum frame zstd ainda não recomprimido.
func (s *Store) coldTodo(ids []uint32) ([]uint32, error) {
	var out []uint32
	for _, id := range ids {
		fs, err := s.segFrames(id)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			if f.codec == codecZstd {
				out = append(out, id)
				break
			}
		}
	}
	return out, nil
}

// Recompress recomprime no zstd 19 os frames de até maxBytes de segmentos
// (uma rodada; chame de novo enquanto Remaining > 0). Segura o depósito
// durante a rodada: rodadas curtas deixam o add entrar entre elas.
func (s *Store) Recompress(maxBytes int64) (ColdResult, error) {
	var res ColdResult
	if !haveZstdMax {
		return res, errors.New("recompressão no nível 19 precisa do binário compilado com cgo")
	}
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
	ids, err := s.segIDs()
	if err != nil {
		return res, err
	}
	todo, err := s.coldTodo(ids)
	if err != nil || len(todo) == 0 {
		return res, err
	}
	// a rodada: segmentos inteiros até maxBytes
	var pick []uint32
	var sum int64
	for _, id := range todo {
		fi, err := os.Stat(s.segPath(id))
		if err != nil {
			return res, err
		}
		if len(pick) > 0 && sum+fi.Size() > maxBytes {
			break
		}
		pick = append(pick, id)
		sum += fi.Size()
	}
	res.Remaining = len(todo) - len(pick)
	res.Before = sum
	type fkey struct {
		seg uint32
		off uint64
	}
	byFrame := map[fkey][]Hash{}
	for h, l := range idx {
		byFrame[fkey{l.seg, l.frameOff}] = append(byFrame[fkey{l.seg, l.frameOff}], h)
	}
	w, err := s.openSegWriter(true)
	if err != nil {
		return res, err
	}
	type job struct {
		k     fkey
		hd    frameHdr
		comp  []byte
		codec byte
		out   []byte
	}
	workers := min(max(s.opt.Workers, 1), 4) // nível 19 usa bastante RAM por tarefa
	var jobs []*job
	flush := func() error {
		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		for _, j := range jobs {
			j.codec, j.out = j.hd.codec, j.comp
			if j.hd.codec != codecZstd {
				continue
			}
			j.codec = codecZstdMax
			wg.Add(1)
			sem <- struct{}{}
			go func(j *job) {
				defer wg.Done()
				defer func() { <-sem }()
				raw, err := s.dec.DecodeAll(j.comp, make([]byte, 0, j.hd.rawLen))
				if err != nil || len(raw) != int(j.hd.rawLen) {
					return // fica como está; a verificação acusa
				}
				if z := zstdMax(raw); z != nil && len(z) < len(j.comp) {
					j.out = z
				}
			}(j)
		}
		wg.Wait()
		for _, j := range jobs {
			seg, off, err := w.writeFrame(j.codec, int(j.hd.rawLen), j.out)
			if err != nil {
				return err
			}
			res.After += int64(frameHdrLen + len(j.out) + frameTailLen)
			if j.codec == codecZstdMax && j.hd.codec == codecZstd {
				res.Frames++
			}
			for _, h := range byFrame[j.k] {
				l := idx[h]
				idx[h] = loc{seg: seg, frameOff: off, offInFrame: l.offInFrame, length: l.length}
			}
		}
		jobs = jobs[:0]
		return nil
	}
	for _, id := range pick {
		fs, err := s.segFrames(id)
		if err != nil {
			w.close()
			return res, err
		}
		for _, f := range fs {
			k := fkey{id, f.off}
			if len(byFrame[k]) == 0 {
				continue // frame sem bloco no índice: lixo, sai junto
			}
			hd, comp, err := s.readRawFrame(id, f.off)
			if err != nil {
				w.close()
				return res, err
			}
			jobs = append(jobs, &job{k: k, hd: hd, comp: comp})
			if len(jobs) >= 4*workers {
				if err := flush(); err != nil {
					w.close()
					return res, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		w.close()
		return res, err
	}
	if err := w.close(); err != nil {
		return res, err
	}
	// índice novo por rename atômico; só então os segmentos velhos saem
	buf := make([]byte, 0, len(idx)*idxRecLen)
	for h, l := range idx {
		buf = append(buf, encodeIdx(h, l)...)
	}
	if err := writeFileSync(s.idxPath(), buf); err != nil {
		return res, err
	}
	s.dropSegReaders()
	s.cache.clear()
	for _, id := range pick {
		if err := os.Remove(s.segPath(id)); err == nil {
			res.Segments++
		}
	}
	syncDir(filepath.Join(s.dir, "seg"))
	s.mu.Lock()
	s.idxIno = 0
	err = s.refreshLocked(true)
	s.mu.Unlock()
	return res, err
}
