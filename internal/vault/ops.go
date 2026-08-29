package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// AddOptions controla o comportamento de importação.
type AddOptions struct {
	CDC        bool   // --cdc (Content-Defined Chunking)
	MultiPass  bool   // --multi-pass
	Encrypt    string // --encrypt
	Brain      string // codebook para pack ("" = default do cofre)
	NoVerify   bool   // pular verificação pós-pack
	ForceTrain bool   // retreinar o codebook do cofre a partir da origem
	NoBrain    bool   // expert: empacotar sem codebook (não-confiável no V24)
}

// ImportResult resume a importação de um arquivo.
type ImportResult struct {
	Entry   *Entry
	Dedup   bool   // conteúdo já existia no cofre
	Trained bool   // codebook do cofre foi treinado agora
	Msg     string // observação extra
}

// Add importa um arquivo (ou diretório, recursivamente) para o cofre.
func (s *Store) Add(src string, opt AddOptions) ([]ImportResult, error) {
	src = ExpandHome(src)
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(src)

	if err := s.resolveBrain(src, &opt); err != nil {
		return nil, err
	}

	var results []ImportResult
	if info.IsDir() {
		var files []string
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.Type().IsRegular() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		for _, f := range files {
			rel, _ := filepath.Rel(src, f)
			r, aerr := s.addFile(f, base, rel, true, opt)
			if aerr != nil {
				fmt.Fprintf(os.Stderr, "! %s: %v\n", f, aerr)
				continue
			}
			results = append(results, r)
		}
	} else {
		r, aerr := s.addFile(src, base, base, false, opt)
		if aerr != nil {
			return nil, aerr
		}
		results = append(results, r)
	}
	if err := s.Save(); err != nil {
		return results, err
	}
	return results, nil
}

// resolveBrain resolve o codebook para importação: bootstrap automático,
// default do cofre ou erro se não houver (pack sem brain não é confiável no V24).
func (s *Store) resolveBrain(src string, opt *AddOptions) error {
	if opt.Brain == "" && (opt.ForceTrain || s.DefaultBrainPath() == "") {
		cb, err := s.TrainBrain(filepath.Dir(src), 0)
		if err != nil {
			// fallback: semente com dados do sistema (codebook só afeta o
			// ratio; lossless é garantido pelo delta XOR do formato)
			cb, err = s.SeedBrainFromSystem()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "! bootstrap do codebook falhou (%v); seguindo sem codebook\n", err)
		} else {
			opt.Brain = cb
			fmt.Printf("✔ codebook do cofre pronto: %s\n", cb)
		}
	} else if opt.Brain == "" {
		opt.Brain = s.DefaultBrainPath()
	}
	if opt.Brain == "" && !opt.NoBrain {
		return errors.New("sem codebook disponível: no V24, pack sem codebook não é lossless confiável ao desempacotar; rode 'crom-ar train -i <dir>' ou use --no-brain se souber o que está fazendo")
	}
	return nil
}

// SeedBrainFromSystem treina um codebook com dados abundantes do próprio
// sistema (binários variados) — usado quando os dados do usuário ainda são
// poucos para o trainer extrair padrões.
func (s *Store) SeedBrainFromSystem() (string, error) {
	candidates := []string{
		filepath.Dir(s.Cfg.CrompressorBin),
		"/usr/bin",
		"/usr/lib",
		"/usr/share/doc",
	}
	for _, dir := range candidates {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		cb, err := s.TrainBrain(dir, 0)
		if err == nil {
			return cb, nil
		}
	}
	return "", errors.New("nenhuma fonte de treinamento disponível")
}

// AddFileOpts descreve a importação de um único arquivo pelo FUSE.
type AddFileOpts struct {
	Src    string // arquivo fonte (spool tmp)
	Folder string // pasta virtual de destino
	Name   string // nome de exibição
	Rel    string // relPath dentro do grupo ("" = Name)
	Group  string // grupo de pacote ("" = Name)
	IsDir  bool   // true = membro de pacote (extract recria a árvore)
	Opt    AddOptions
}

// ImportFile importa um único arquivo posicionando-o no cofre.
// Sobrescreve a entrada que ocupa o mesmo caminho.
func (s *Store) ImportFile(o AddFileOpts) (*Entry, error) {
	src := ExpandHome(o.Src)
	folder := NormalizeFolderInput(o.Folder)
	name := filepath.Base(o.Name)
	rel := o.Rel
	if rel == "" {
		rel = name
	}
	group := o.Group
	if group == "" {
		group = name
	}
	if err := s.resolveBrain(src, &o.Opt); err != nil {
		return nil, err
	}
	target := path.Join(folder, name)
	s.mu.Lock()
	if old := s.entryByPathLocked(target); old != nil {
		if _, err := s.removeLocked([]*Entry{old}); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()
	r, err := s.addFile(src, group, rel, o.IsDir, o.Opt)
	if err != nil {
		return nil, err
	}
	e := r.Entry
	e.Folder = folder
	e.Path = target
	if err := s.Save(); err != nil {
		return e, err
	}
	return e, nil
}

func (s *Store) addFile(src, group, rel string, isDir bool, opt AddOptions) (ImportResult, error) {
	hash, size, err := SHA256File(src)
	if err != nil {
		return ImportResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Dedup por conteúdo: .crom já existe, registra entrada apontando para ele.
	var refEntry *Entry
	for _, e := range s.Man.Entries {
		if e.ID == hash {
			if e.SourcePath == src && e.Group == group && e.RelPath == rel {
				return ImportResult{Entry: e, Dedup: true, Msg: "já importado"}, nil
			}
			refEntry = e
			break
		}
	}
	if refEntry != nil || s.HasContent(hash) {
		cromName := hash + ".crom"
		var cromSize int64
		if st, err := os.Stat(filepath.Join(s.Cfg.VaultDir, cromName)); err == nil {
			cromSize = st.Size()
		}
		e := s.newEntry(hash, src, group, rel, isDir, size, cromName, cromSize, opt.Brain, opt)
		e.Verified = true
		s.Man.Entries = append(s.Man.Entries, e)
		msg := "dedup: .crom já no cofre"
		if refEntry == nil {
			msg = "dedup: conteúdo idêntico já importado"
		}
		return ImportResult{Entry: e, Dedup: true, Msg: msg}, nil
	}

	cromName := hash + ".crom"
	cromPath := filepath.Join(s.Cfg.VaultDir, cromName)
	args := []string{"pack", "-i", src, "-o", cromPath}
	var flags []string
	if opt.CDC {
		args = append(args, "--cdc")
		flags = append(flags, "--cdc")
	}
	if opt.MultiPass {
		args = append(args, "--multi-pass")
		flags = append(flags, "--multi-pass")
	}
	if opt.Brain != "" {
		args = append(args, "-c", opt.Brain)
	}
	if opt.Encrypt != "" {
		args = append(args, "--encrypt", opt.Encrypt)
	}
	out, err := s.Run(args...)
	if err != nil {
		os.Remove(cromPath)
		return ImportResult{}, fmt.Errorf("pack: %v", err)
	}
	st, err := os.Stat(cromPath)
	if err != nil {
		return ImportResult{}, err
	}

	e := s.newEntry(hash, src, group, rel, isDir, size, cromName, st.Size(), opt.Brain, opt)
	e.HitRate = parseHitRate(out)

	if !opt.NoVerify {
		if err := s.VerifyEntry(e, opt.Encrypt); err != nil {
			os.Remove(cromPath)
			return ImportResult{}, fmt.Errorf("verificação lossless falhou: %v", err)
		}
		e.Verified = true
	}
	s.Man.Entries = append(s.Man.Entries, e)
	return ImportResult{Entry: e}, nil
}

func (s *Store) newEntry(hash, src, group, rel string, isDir bool, size int64, cromName string, cromSize int64, brain string, opt AddOptions) *Entry {
	folder := "/"
	return &Entry{
		UID: randomUID(),
		ID: hash, Name: filepath.Base(rel), Group: group, RelPath: rel, Folder: folder,
		Path: canonicalPath(folder, rel), IsDir: isDir,
		SourcePath: src, Size: size, CromFile: cromName, CromSize: cromSize,
		Codebook: brain, PackFlags: opt.PackFlagsList(), PackedAt: s.timeNow(),
	}
}

// parseHitRate extrai "Hit Rate: X% dos chunks" do output do pack.
func parseHitRate(out string) float64 {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Hit Rate:") {
			continue
		}
		i := strings.Index(line, ":")
		rest := strings.TrimSpace(line[i+1:])
		j := strings.IndexAny(rest, "0123456789")
		if j < 0 {
			return 0
		}
		k := j
		for k < len(rest) && (rest[k] == '.' || (rest[k] >= '0' && rest[k] <= '9')) {
			k++
		}
		var v float64
		fmt.Sscanf(rest[j:k], "%f", &v)
		return v
	}
	return 0
}

// PackFlagsList serializa as opções de pack da entrada.
func (o AddOptions) PackFlagsList() []string {
	var f []string
	if o.CDC {
		f = append(f, "--cdc")
	}
	if o.MultiPass {
		f = append(f, "--multi-pass")
	}
	if o.Encrypt != "" {
		f = append(f, "--encrypt")
	}
	return f
}

// VerifyEntry faz unpack em temp e compara SHA-256 com o conteúdo original.
func (s *Store) VerifyEntry(e *Entry, pass string) error {
	tmp, err := os.CreateTemp("", "crom-ar-verify-*")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	cb := e.Codebook
	if cb == "" {
		cb = s.DefaultBrainPath()
	}
	if cb == "" {
		return errors.New("nenhum codebook disponível para unpack (V24 exige --codebook)")
	}
	args := []string{"unpack", "-i", s.CromPath(e), "-o", tmp.Name(), "-c", cb}
	if pass != "" {
		args = append(args, "--encrypt", pass)
	}
	if _, err := s.Run(args...); err != nil {
		return err
	}
	h, _, err := SHA256File(tmp.Name())
	if err != nil {
		return err
	}
	if h != e.ID {
		return fmt.Errorf("sha256 divergente: %s != %s", h, e.ID)
	}
	return nil
}

// TrainBrain treina o codebook do cofre (brains/vault.cromdb).
func (s *Store) TrainBrain(srcDir string, size int) (string, error) {
	out := filepath.Join(s.Cfg.BrainsDir, "vault.cromdb")
	args := []string{"train", "-i", ExpandHome(srcDir), "-o", out}
	if size > 0 {
		args = append(args, "--size", fmt.Sprint(size))
	}
	if _, err := s.Run(args...); err != nil {
		return "", err
	}
	return out, nil
}

// TrainBrainUpdate faz atualização incremental do codebook do cofre
// com os dados do diretório informado.
func (s *Store) TrainBrainUpdate(srcDir string, size int) (string, error) {
	out := filepath.Join(s.Cfg.BrainsDir, "vault.cromdb")
	if _, err := os.Stat(out); err != nil {
		return s.TrainBrain(srcDir, size)
	}
	args := []string{"train", "-i", ExpandHome(srcDir), "--update", out}
	if size > 0 {
		args = append(args, "--size", fmt.Sprint(size))
	}
	if _, err := s.Run(args...); err != nil {
		return "", err
	}
	return out, nil
}

// Match localiza entradas por id (prefixo), nome, grupo ou caminho.
func (s *Store) Match(term string) []*Entry {
	term = strings.ToLower(term)
	var exact, partial []*Entry
	for _, e := range s.Man.Entries {
		if e.ID == term || strings.EqualFold(e.Name, term) || strings.EqualFold(e.Group, term) ||
			strings.EqualFold(e.RelPath, term) || strings.HasPrefix(e.ID, term) {
			exact = append(exact, e)
			continue
		}
		if strings.Contains(strings.ToLower(e.Name), term) ||
			strings.Contains(strings.ToLower(e.Group), term) ||
			strings.Contains(strings.ToLower(e.RelPath), term) {
			partial = append(partial, e)
		}
	}
	return append(exact, partial...)
}

// ExtractOptions controla a extração.
type ExtractOptions struct {
	OutDir string
	Force  bool
	EncKey string
}

// ExtractResult é o resultado da extração de uma entrada.
type ExtractResult struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Ok     bool   `json:"ok"`
	Err    string `json:"error,omitempty"`
}

// Extract recompõe os arquivos no HD local (estilo WinRAR).
func (s *Store) Extract(entries []*Entry, opt ExtractOptions) ([]ExtractResult, error) {
	out := ExpandHome(opt.OutDir)
	if out == "" {
		out = "crom-restore"
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	var results []ExtractResult
	seen := map[string]bool{}
	for _, e := range entries {
		var target string
		if e.IsDir {
			target = filepath.Join(out, e.Group, e.RelPath)
		} else {
			target = filepath.Join(out, e.RelPath)
		}
		key := e.ID + "|" + target
		if seen[key] {
			continue
		}
		seen[key] = true
		res := ExtractResult{UID: e.UID, Name: e.RelPath, Target: target}
		if _, err := os.Stat(target); err == nil && !opt.Force {
			res.Err = "existe (use --force para sobrescrever)"
			results = append(results, res)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			res.Err = err.Error()
			results = append(results, res)
			continue
		}
		cb := e.Codebook
		if cb == "" {
			cb = s.DefaultBrainPath()
		}
		if cb == "" {
			res.Err = "nenhum codebook disponível para unpack"
			results = append(results, res)
			continue
		}
		args := []string{"unpack", "-i", s.CromPath(e), "-o", target, "-c", cb}
		if opt.EncKey != "" {
			args = append(args, "--encrypt", opt.EncKey)
		}
		if _, err := s.Run(args...); err != nil {
			res.Err = err.Error()
			results = append(results, res)
			continue
		}
		if h, _, err := SHA256File(target); err != nil || h != e.ID {
			res.Err = "corrompido ao extrair (hash divergente)"
			results = append(results, res)
			continue
		}
		res.Ok = true
		results = append(results, res)
	}
	return results, nil
}

// Remove remove entradas do manifest; apaga o .crom se não houver mais referências.
func (s *Store) Remove(entries []*Entry) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(entries)
}

func (s *Store) removeLocked(entries []*Entry) (int, error) {
	removed := map[string]bool{}
	for _, e := range entries {
		removed[e.UID] = true
	}
	var keep []*Entry
	for _, e := range s.Man.Entries {
		if removed[e.UID] {
			continue
		}
		keep = append(keep, e)
	}
	removedN := len(s.Man.Entries) - len(keep)
	s.Man.Entries = keep

	// apaga .crom órfãos
	refs := map[string]int{}
	for _, e := range s.Man.Entries {
		refs[e.CromFile]++
	}
	for _, e := range entries {
		if refs[e.CromFile] == 0 {
			if err := os.Remove(s.CromPath(e)); err == nil {
				fmt.Printf("🗑  %s liberado do cofre\n", e.CromFile)
			}
		}
	}
	return removedN, s.Save()
}

// Stats resume o cofre.
type Stats struct {
	Files       int64   `json:"files"`
	Unique      int64   `json:"unique_contents"`
	Original    int64   `json:"original_bytes"`
	UniqueBytes int64   `json:"unique_bytes"`
	Stored      int64   `json:"stored_bytes"`
	DedupSaved  int64   `json:"dedup_saved_bytes"`
	AvgRatio    float64 `json:"avg_ratio_pct"`
	VaultOnDisk int64   `json:"vault_on_disk_bytes"`
}

// ComputeStats calcula estatísticas do cofre.
func (s *Store) ComputeStats() Stats {
	st := Stats{}
	perContent := map[string]bool{}
	cromFiles := map[string]bool{}
	for _, e := range s.Man.Entries {
		st.Files++
		st.Original += e.Size
		if perContent[e.ID] {
			st.DedupSaved += e.Size
		} else {
			perContent[e.ID] = true
			st.UniqueBytes += e.Size
			st.Unique++
		}
		if !cromFiles[e.CromFile] {
			cromFiles[e.CromFile] = true
			st.Stored += e.CromSize
		}
	}
	if s.Cfg != nil && s.Cfg.VaultDir != "" {
		_ = filepath.WalkDir(s.Cfg.VaultDir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if i, err := d.Info(); err == nil {
					st.VaultOnDisk += i.Size()
				}
			}
			return nil
		})
	}
	var ratioSum float64
	var ratioN int
	for _, e := range s.Man.Entries {
		if e.Size > 0 {
			ratioSum += float64(e.CromSize) / float64(e.Size) * 100
			ratioN++
		}
	}
	if ratioN > 0 {
		st.AvgRatio = ratioSum / float64(ratioN)
	}
	return st
}

// HumanBytes formata bytes para leitura humana.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
