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
	"strings"
	"syscall"
)

// Receitas num log só (recipes.log), no lugar de um arquivo por conteúdo:
// com milhares de arquivos pequenos, um .rcp de 100 bytes ocupava um bloco
// de 4 KB do sistema de arquivos e o cofre ficava maior que a origem.
//
// Registro: "CRL1" | u32 tamanho do corpo | u32 CRC32C do corpo | corpo
// Corpo:    op | hash 32 bytes | receita codificada (só op 1)
//   op 1 = grava, 2 = apaga, 3 = conteúdo de um bloco só (o hash do conteúdo
//   é o hash do bloco: a receita sai do índice, sem gastar 36+ bytes)
//
// Só acrescenta (com flock), como o index.log. O GC reescreve o log só com as
// receitas vivas, por rename atômico. Um registro corrompido é pulado e marca
// o log como suspeito: o GC então se recusa a apagar blocos (não dá para saber
// de quem era a receita perdida).

const (
	rcpMagic  = "CRL1"
	rcpHdrLen = 12
	opPut     = 1
	opDel     = 2
	opOne     = 3
)

type rloc struct {
	off int64 // posição da receita codificada dentro do log (-1 = um bloco só)
	n   uint32
}

// recipeRecord escolhe o registro mais curto para a receita.
func recipeRecord(r *Recipe) []byte {
	if len(r.Chunks) == 1 && r.Chunks[0].Hash == r.Hash {
		return rcpRecord(opOne, r.Hash, nil)
	}
	return rcpRecord(opPut, r.Hash, encodeRecipe(r))
}

func (s *Store) rcpPath() string { return filepath.Join(s.dir, "recipes.log") }

func rcpRecord(op byte, h Hash, enc []byte) []byte {
	body := make([]byte, 0, 33+len(enc))
	body = append(body, op)
	body = append(body, h[:]...)
	body = append(body, enc...)
	b := make([]byte, 0, rcpHdrLen+len(body))
	b = append(b, rcpMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(body)))
	b = binary.LittleEndian.AppendUint32(b, crc32.Checksum(body, crcTab))
	return append(b, body...)
}

// refreshRecipesLocked lê do recipes.log o que foi acrescentado. Com
// writer=true, um final cortado por queda é truncado. Chamar com s.mu.
func (s *Store) refreshRecipesLocked(writer bool) error {
	f, err := os.Open(s.rcpPath())
	if os.IsNotExist(err) {
		s.rcp, s.rcpOff, s.rcpIno, s.rcpDead, s.rcpBad = map[Hash]rloc{}, 0, 0, 0, false
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
	ino := st.Sys().(*syscall.Stat_t).Ino
	if ino != s.rcpIno || st.Size() < s.rcpOff { // reescrito pelo GC: recarrega
		s.rcp, s.rcpOff, s.rcpIno, s.rcpDead, s.rcpBad = map[Hash]rloc{}, 0, ino, 0, false
	}
	if st.Size() == s.rcpOff {
		return nil
	}
	buf := make([]byte, st.Size()-s.rcpOff)
	if _, err := f.ReadAt(buf, s.rcpOff); err != nil && err != io.EOF {
		return err
	}
	pos := 0
	for pos < len(buf) {
		rest := buf[pos:]
		if len(rest) < rcpHdrLen || string(rest[:4]) != rcpMagic {
			// lixo: procura o próximo registro; se não houver, é final cortado
			nx := bytes.Index(rest[1:], []byte(rcpMagic))
			if nx < 0 {
				break
			}
			s.rcpBad = true
			pos += 1 + nx
			continue
		}
		n := int(binary.LittleEndian.Uint32(rest[4:]))
		if rcpHdrLen+n > len(rest) {
			break // registro pela metade no fim
		}
		body := rest[rcpHdrLen : rcpHdrLen+n]
		if n < 33 || crc32.Checksum(body, crcTab) != binary.LittleEndian.Uint32(rest[8:]) {
			s.rcpBad = true
			pos += rcpHdrLen + n
			continue
		}
		var h Hash
		copy(h[:], body[1:33])
		if _, ok := s.rcp[h]; ok {
			s.rcpDead++
		}
		switch body[0] {
		case opPut:
			s.rcp[h] = rloc{off: s.rcpOff + int64(pos+rcpHdrLen+33), n: uint32(n - 33)}
		case opOne:
			s.rcp[h] = rloc{off: -1}
		case opDel:
			delete(s.rcp, h)
			s.rcpDead++
		default:
			s.rcpBad = true
		}
		pos += rcpHdrLen + n
	}
	if pos < len(buf) && writer {
		if err := os.Truncate(s.rcpPath(), s.rcpOff+int64(pos)); err != nil {
			return err
		}
	}
	s.rcpOff += int64(pos)
	return nil
}

func (s *Store) appendRecipes(recs []byte) error {
	if len(recs) == 0 {
		return nil
	}
	return appendSync(s.rcpPath(), recs)
}

// appendSync acrescenta ao fim do arquivo e faz fsync.
func appendSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Has diz se o conteúdo h já está no depósito.
func (s *Store) Has(h Hash) bool {
	s.mu.RLock()
	_, ok := s.pend[h]
	if !ok {
		_, ok = s.rcp[h]
	}
	s.mu.RUnlock()
	if ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshLocked(false) != nil {
		return false
	}
	_, ok = s.rcp[h]
	return ok
}

// Recipe lê a receita do conteúdo h.
func (s *Store) Recipe(h Hash) (*Recipe, error) {
	for try := 0; ; try++ {
		s.mu.RLock()
		r, pending := s.pend[h]
		l, ok := s.rcp[h]
		s.mu.RUnlock()
		if pending {
			return r, nil
		}
		if !ok || try > 0 {
			s.mu.Lock()
			if try > 0 {
				s.rcpIno = 0 // força recarga (o log pode ter sido reescrito)
			}
			err := s.refreshLocked(false)
			l, ok = s.rcp[h]
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("conteúdo %s não está no depósito", h.String()[:12])
			}
		}
		if l.off < 0 {
			c, ok := s.lookup(h)
			if !ok {
				return nil, fmt.Errorf("conteúdo %s: bloco único fora do índice", h.String()[:12])
			}
			return &Recipe{Hash: h, Size: int64(c.length), Chunks: []RecipeChunk{{Hash: h, Len: c.length}}}, nil
		}
		b, err := readAtPath(s.rcpPath(), l.off, int(l.n))
		if err == nil {
			if r, err = decodeRecipe(h, b); err == nil {
				return r, nil
			}
		}
		if try > 0 {
			return nil, err
		}
	}
}

func readAtPath(path string, off int64, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

// Contents lista os conteúdos guardados.
func (s *Store) Contents() ([]Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return nil, err
	}
	out := make([]Hash, 0, len(s.rcp))
	for h := range s.rcp {
		out = append(out, h)
	}
	return out, nil
}

// liveRecipes devolve as receitas para marcar blocos vivos. Recusa se o log
// tem registro corrompido (uma receita pode ter se perdido).
func (s *Store) liveRecipes() ([]*Recipe, error) {
	hs, err := s.Contents()
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	bad := s.rcpBad
	s.mu.RUnlock()
	if bad {
		return nil, errors.New("recipes.log tem registro corrompido: GC recusado para não apagar blocos de uma receita perdida")
	}
	out := make([]*Recipe, 0, len(hs))
	for _, h := range hs {
		r, err := s.Recipe(h)
		if err != nil {
			return nil, err // receita ilegível: não arrisca apagar nada
		}
		out = append(out, r)
	}
	return out, nil
}

// removeRecipeLocked marca os conteúdos como apagados, num append e um
// fsync só. Chamar com wmu + flock.
func (s *Store) removeRecipeLocked(hs ...Hash) error {
	s.mu.Lock()
	err := s.refreshLocked(true)
	var recs []byte
	for _, h := range hs {
		delete(s.pend, h)
		if _, ok := s.rcp[h]; ok {
			recs = append(recs, rcpRecord(opDel, h, nil)...)
		}
	}
	s.mu.Unlock()
	if err != nil || len(recs) == 0 {
		return err
	}
	if err := s.appendRecipes(recs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(true)
}

// Remove apaga as receitas dos conteúdos. Os blocos que ficarem sem dono
// saem no GC.
func (s *Store) Remove(hs ...Hash) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	lk, err := s.flock()
	if err != nil {
		return err
	}
	defer funlock(lk)
	return s.removeRecipeLocked(hs...)
}

// compactRecipesLocked reescreve o log só com as receitas vivas, se houver
// registros mortos. Chamar com wmu + flock.
func (s *Store) compactRecipesLocked(rs []*Recipe) error {
	s.mu.RLock()
	dead := s.rcpDead
	s.mu.RUnlock()
	if dead == 0 {
		return nil
	}
	var buf []byte
	for _, r := range rs {
		buf = append(buf, recipeRecord(r)...)
	}
	if err := writeFileSync(s.rcpPath(), buf); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rcpIno = 0
	return s.refreshLocked(true)
}

// migrateRecipeFiles passa o formato antigo (recipes/ab/<sha>.rcp, um
// arquivo por conteúdo) para o recipes.log e apaga os arquivos.
func (s *Store) migrateRecipeFiles() error {
	old := filepath.Join(s.dir, "recipes")
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	lk, err := s.flock()
	if err != nil {
		return err
	}
	defer funlock(lk)
	var files []string
	var buf []byte
	err = filepath.WalkDir(old, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, p)
		if !strings.HasSuffix(p, ".rcp") {
			return nil // .tmp de gravação interrompida
		}
		h, err := ParseHash(strings.TrimSuffix(d.Name(), ".rcp"))
		if err != nil {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if _, err := decodeRecipe(h, b); err != nil {
			return fmt.Errorf("migração de receitas: %s: %w", d.Name(), err)
		}
		buf = append(buf, rcpRecord(opPut, h, b)...)
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	err = s.refreshLocked(true)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.appendRecipes(buf); err != nil {
		return err
	}
	for _, p := range files {
		os.Remove(p)
	}
	return os.RemoveAll(old)
}
