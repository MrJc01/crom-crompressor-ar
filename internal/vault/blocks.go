package vault

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MrJc01/crom-crompressor-ar/internal/store"
)

// StorageBlocks marca entradas guardadas no depósito de blocos (store/).
// Entradas sem Storage são .crom do crompressor (formato anterior, lidas
// normalmente até a migração).
const StorageBlocks = "blocks"

// IsBlocks diz se a entrada mora no depósito de blocos.
func (e *Entry) IsBlocks() bool { return e.Storage == StorageBlocks }

// ContentKey identifica o armazenamento físico da entrada (para não contar
// duas vezes o mesmo conteúdo nas estatísticas).
func (e *Entry) ContentKey() string {
	if e.IsBlocks() {
		return "blocks:" + e.ID
	}
	return e.CromFile
}

// StoreDir é a pasta do depósito de blocos.
func (s *Store) StoreDir() string { return filepath.Join(s.Cfg.Root, "store") }

// Blocks abre (uma vez por processo) o depósito de blocos do cofre.
func (s *Store) Blocks() (*store.Store, error) {
	s.blocksMu.Lock()
	defer s.blocksMu.Unlock()
	if s.blocks != nil {
		return s.blocks, nil
	}
	bs, err := store.Open(s.StoreDir(), store.Options{})
	if err != nil {
		return nil, fmt.Errorf("depósito de blocos: %w", err)
	}
	s.blocks = bs
	return bs, nil
}

func (s *Store) hasBlocks(hash string) bool {
	h, err := store.ParseHash(hash)
	if err != nil {
		return false
	}
	if _, err := os.Stat(s.StoreDir()); err != nil {
		return false
	}
	bs, err := s.Blocks()
	return err == nil && bs.Has(h)
}

// putBlocks grava src no depósito e confere que o conteúdo é o esperado.
func (s *Store) putBlocks(src, wantHash string, verify bool) (store.PutResult, error) {
	bs, err := s.Blocks()
	if err != nil {
		return store.PutResult{}, err
	}
	res, err := bs.PutFile(src)
	if err != nil {
		return res, err
	}
	if res.Hash.String() != wantHash {
		return res, fmt.Errorf("o arquivo mudou durante a importação (sha256 %s ≠ %s)", res.Hash.String()[:12], wantHash[:12])
	}
	if verify {
		if err := bs.Verify(res.Hash); err != nil {
			return res, fmt.Errorf("verificação lossless falhou: %v", err)
		}
	}
	return res, nil
}

// Content é um conteúdo aberto para leitura aleatória.
type Content interface {
	io.ReaderAt
	Size() int64
}

// OpenBlocks abre o conteúdo de uma entrada do depósito, sem descomprimir
// o arquivo inteiro: cada ReadAt descomprime só os frames do trecho.
func (s *Store) OpenBlocks(e *Entry) (Content, error) {
	h, err := store.ParseHash(e.ID)
	if err != nil {
		return nil, err
	}
	bs, err := s.Blocks()
	if err != nil {
		return nil, err
	}
	return bs.Open(h)
}

// MaterializeTo grava o conteúdo original da entrada em dst (qualquer
// formato de armazenamento), conferindo o SHA-256.
func (s *Store) MaterializeTo(e *Entry, dst string) error {
	if e.IsBlocks() {
		h, err := store.ParseHash(e.ID)
		if err != nil {
			return err
		}
		bs, err := s.Blocks()
		if err != nil {
			return err
		}
		return bs.Extract(h, dst, 0)
	}
	cb := e.Codebook
	if cb == "" {
		cb = s.DefaultBrainPath()
	}
	args := []string{"unpack", "-i", s.CromPath(e), "-o", dst}
	if cb != "" {
		args = append(args, "-c", cb)
	}
	if _, err := s.Run(args...); err != nil {
		return err
	}
	if h, _, err := SHA256File(dst); err != nil || h != e.ID {
		return fmt.Errorf("conteúdo de %s não bate com o sha256 ao extrair", e.Name)
	}
	return nil
}

// releaseBlocks apaga do depósito os conteúdos que ficaram sem entrada e
// compacta segmentos com muito espaço morto. Chamado com s.mu tomado.
func (s *Store) releaseBlocks(removed []*Entry) {
	live := map[string]bool{}
	for _, e := range s.Man.Entries {
		if e.IsBlocks() {
			live[e.ID] = true
		}
	}
	var gone int
	for _, e := range removed {
		if !e.IsBlocks() || live[e.ID] {
			continue
		}
		h, err := store.ParseHash(e.ID)
		if err != nil {
			continue
		}
		var rerr error
		if s.activeBatch != nil { // um Add de pasta está gravando: usa o lote dele
			rerr = s.activeBatch.Remove(h)
		} else {
			bs, err := s.Blocks()
			if err != nil {
				return
			}
			rerr = bs.Remove(h)
		}
		if rerr == nil {
			live[e.ID] = true // não remover duas vezes
			gone++
		}
	}
	if gone == 0 || s.activeBatch != nil { // GC fica para depois do lote (doctor --prune)
		return
	}
	bs, _ := s.Blocks()
	if res, err := bs.GC(0.3); err != nil {
		fmt.Fprintf(os.Stderr, "! GC do depósito: %v\n", err)
	} else if res.FreedBytes > 0 {
		fmt.Printf("🗑  %s liberados do depósito\n", HumanBytes(res.FreedBytes))
	}
}

// GCBlocks roda a coleta do depósito (doctor --prune).
func (s *Store) GCBlocks(minDead float64) (store.GCResult, error) {
	if _, err := os.Stat(s.StoreDir()); err != nil {
		return store.GCResult{}, nil
	}
	bs, err := s.Blocks()
	if err != nil {
		return store.GCResult{}, err
	}
	return bs.GC(minDead)
}

func dirSize(p string) int64 {
	var n int64
	filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// InfoText descreve como a entrada está guardada (texto para CLI e painel).
func (s *Store) InfoText(e *Entry) (string, error) {
	if !e.IsBlocks() {
		args := []string{"info", "-i", s.CromPath(e)}
		if e.Codebook != "" {
			args = append(args, "-c", e.Codebook)
		}
		return s.Run(args...)
	}
	h, err := store.ParseHash(e.ID)
	if err != nil {
		return "", err
	}
	bs, err := s.Blocks()
	if err != nil {
		return "", err
	}
	r, err := bs.Recipe(h)
	if err != nil {
		return "", err
	}
	uniq := map[store.Hash]bool{}
	for _, c := range r.Chunks {
		uniq[c.Hash] = true
	}
	st, _ := bs.Stats()
	var b strings.Builder
	fmt.Fprintf(&b, "═══ %s ═══\n", e.Path)
	fmt.Fprintf(&b, "  armazenamento:     depósito de blocos (%s)\n", s.StoreDir())
	fmt.Fprintf(&b, "  sha256:            %s\n", e.ID)
	fmt.Fprintf(&b, "  tamanho original:  %s\n", HumanBytes(r.Size))
	fmt.Fprintf(&b, "  blocos:            %d (%d distintos, média %s)\n", len(r.Chunks), len(uniq), HumanBytes(r.Size/int64(max(1, len(r.Chunks)))))
	fmt.Fprintf(&b, "  acrescentou disco: %s ao importar (%.1f%% do original)\n", HumanBytes(e.CromSize), pct(e.CromSize, r.Size))
	fmt.Fprintf(&b, "  blocos que já existiam no cofre ao importar: %.1f%%\n", e.HitRate)
	fmt.Fprintf(&b, "  depósito inteiro:  %d conteúdos, %d blocos únicos, %s em disco\n", st.Contents, st.UniqueChunks, HumanBytes(st.DiskBytes))
	return b.String(), nil
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

// Grep procura term (sem diferenciar maiúsculas) nas linhas do conteúdo,
// lendo direto do depósito. Devolve até max linhas.
func (s *Store) Grep(e *Entry, term string, max int) ([]string, error) {
	c, err := s.OpenBlocks(e)
	if err != nil {
		return nil, err
	}
	low := strings.ToLower(term)
	sc := bufio.NewScanner(io.NewSectionReader(c, 0, c.Size()))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var out []string
	for sc.Scan() && len(out) < max {
		if line := sc.Text(); strings.Contains(strings.ToLower(line), low) {
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			out = append(out, line)
		}
	}
	return out, nil
}

type batchItem struct {
	e      *Entry
	r      *store.PutResult // nil = dedup (nada novo gravado)
	verify bool
}

// entryByID devolve a primeira entrada com o conteúdo hash (O(1) com cache).
func (s *Store) entryByID(hash string) *Entry {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	if s.byID == nil {
		s.byID = make(map[string]*Entry, len(s.Man.Entries))
		for _, e := range s.Man.Entries {
			if _, ok := s.byID[e.ID]; !ok {
				s.byID[e.ID] = e
			}
		}
	}
	return s.byID[hash]
}

func (s *Store) beginBatch() error {
	bs, err := s.Blocks()
	if err != nil {
		return err
	}
	b, err := bs.Begin()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.activeBatch, s.batchPend = b, nil
	s.mu.Unlock()
	return nil
}

func (s *Store) abortBatch() {
	s.mu.Lock()
	b := s.activeBatch
	s.activeBatch, s.batchPend = nil, nil
	s.mu.Unlock()
	if b != nil {
		b.Abort()
	}
}

// addToBatch grava no lote aberto. Chamado com s.mu tomado.
func (s *Store) addToBatch(src, hash, group, rel string, isDir bool, size int64, opt AddOptions) (ImportResult, error) {
	f, err := os.Open(src)
	if err != nil {
		return ImportResult{}, err
	}
	r, err := s.activeBatch.Put(f)
	f.Close()
	if err != nil {
		return ImportResult{}, err
	}
	if r.Hash.String() != hash {
		return ImportResult{}, fmt.Errorf("o arquivo mudou durante a importação (sha256 %s ≠ %s)", r.Hash.String()[:12], hash[:12])
	}
	e := s.newEntry(hash, src, group, rel, isDir, size, "", 0, "", opt)
	e.Storage = StorageBlocks
	e.PackFlags = nil
	it := batchItem{e: e, r: r, verify: !opt.NoVerify}
	if opt.batched {
		s.batchPend = append(s.batchPend, it)
		return ImportResult{Entry: e}, nil
	}
	// gravação de fora do Add (FUSE/painel durante um import grande):
	// commit agora, para a entrada valer já
	if err := s.activeBatch.Commit(); err != nil {
		return ImportResult{}, err
	}
	if err := s.settleItem(&it); err != nil {
		return ImportResult{}, err
	}
	s.appendEntry(e)
	return ImportResult{Entry: e}, nil
}

// settleItem preenche os números da entrada e verifica o conteúdo relendo
// do depósito (depois do commit).
func (s *Store) settleItem(it *batchItem) error {
	if it.r == nil {
		return nil
	}
	it.e.CromSize = it.r.StoredDisk
	if it.r.Chunks > 0 {
		it.e.HitRate = 100 * float64(it.r.Chunks-it.r.NewChunks) / float64(it.r.Chunks)
	}
	if !it.verify {
		return nil
	}
	h, err := store.ParseHash(it.e.ID)
	if err != nil {
		return err
	}
	bs, err := s.Blocks()
	if err != nil {
		return err
	}
	if err := bs.Verify(h); err != nil {
		return fmt.Errorf("verificação lossless falhou: %v", err)
	}
	it.e.Verified = true
	return nil
}

// finishBatch faz o commit do lote do Add, verifica cada conteúdo novo e só
// então registra as entradas no manifest.
func (s *Store) finishBatch(results []ImportResult) ([]ImportResult, error) {
	s.mu.Lock()
	b, pend := s.activeBatch, s.batchPend
	s.activeBatch, s.batchPend = nil, nil
	s.mu.Unlock()
	if b == nil {
		return results, nil
	}
	if err := b.Close(); err != nil {
		return nil, fmt.Errorf("gravar o lote no depósito: %w", err)
	}
	bad := map[*Entry]bool{}
	checked := map[string]error{}
	for i := range pend {
		it := &pend[i]
		if it.r == nil {
			continue
		}
		if err, ok := checked[it.e.ID]; ok { // mesmo conteúdo já verificado
			it.verify = false
			if err != nil {
				bad[it.e] = true
				continue
			}
		}
		err := s.settleItem(it)
		checked[it.e.ID] = err
		if err != nil {
			fmt.Fprintf(os.Stderr, "! %s: %v\n", it.e.RelPath, err)
			bad[it.e] = true
		} else if !it.verify && !it.e.Verified {
			it.e.Verified = checked[it.e.ID] == nil
		}
	}
	s.mu.Lock()
	for _, it := range pend {
		if !bad[it.e] {
			s.appendEntry(it.e)
		}
	}
	s.mu.Unlock()
	out := results[:0]
	for _, r := range results {
		if !bad[r.Entry] {
			out = append(out, r)
		}
	}
	return out, nil
}
