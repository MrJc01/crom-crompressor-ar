// Package vault implementa o cofre local de arquivos .crom do crom-ar:
// manifest, config, dedup por SHA-256 e operações add/extract/rm/stats.
package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/MrJc01/crom-crompressor-ar/internal/store"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ArchiveRoot é a pasta virtual que expõe os "pacotes" (grupos) no FUSE:
// cada subpasta de /arquivos é um arquivo .crom coletivo no estilo WinRAR.
const ArchiveRoot = "/arquivos"

// Config é a configuração do cofre (config.json).
type Config struct {
	Root           string `json:"root"`
	VaultDir       string `json:"vault_dir"`
	BrainsDir      string `json:"brains_dir"`
	CrompressorBin string `json:"crompressor_bin"`
	DefaultBrain   string `json:"default_brain"`
}

// Entry é um arquivo lógico importado no cofre.
// Várias entradas podem apontar para o mesmo .crom (dedup por conteúdo).
type Entry struct {
	UID        string    `json:"uid"` // identificador estável para a GUI/API
	ID         string    `json:"id"`  // sha256 do conteúdo (chave de dedup)
	Name       string    `json:"name"`
	Group      string    `json:"group"`
	RelPath    string    `json:"rel_path"`
	Folder     string    `json:"folder"` // pasta virtual dentro do cofre ("/" = raiz)
	Path       string    `json:"path"`   // caminho canônico de exibição (FUSE e GUI)
	IsDir      bool      `json:"is_dir"`
	SourcePath string    `json:"source_path"`
	Size       int64     `json:"size"`
	CromFile   string    `json:"crom_file"`         // nome do arquivo dentro de VaultDir
	CromSize   int64     `json:"crom_size"`         // bytes que esta entrada acrescentou ao disco
	Storage    string    `json:"storage,omitempty"` // "blocks" = depósito de blocos; "" = .crom do crompressor
	Codebook   string    `json:"codebook"`          // "" = empacotado sem codebook
	HitRate    float64   `json:"hit_rate"`          // % de chunks que caíram no codebook
	Mode       uint32    `json:"mode,omitempty"`    // permissões (0 = 0644; exec bit preservado)
	PackFlags  []string  `json:"pack_flags"`
	PackedAt   time.Time `json:"packed_at"`
	Verified   bool      `json:"verified"`
}

// Manifest é o índice do cofre (manifest.json).
type Manifest struct {
	Version int      `json:"version"`
	Folders []string `json:"folders"` // pastas virtuais (caminhos absolutos virtuais, "/" é a raiz)
	Entries []*Entry `json:"entries"`
}

// Store agrega config + manifest e o acesso ao binário crompressor.
type Store struct {
	Cfg     *Config
	Man     *Manifest
	manPath string
	timeNow func() time.Time
	mu      sync.Mutex
	sem     chan struct{}
	dataMu  sync.RWMutex // protege as fatias Man.Entries/Man.Folders contra leitores concorrentes (painel web)

	// Consistência residente × CLI: mutações locais não salvas são marcadas
	// (dirty) e as remoções anotadas (deleted); no Save, se o manifest em
	// disco mudou desde a última sincronização, um merge por UID adota o que
	// outro processo escreveu — sem apagar mutações de ninguém (T14a).
	saveMu         sync.Mutex
	lastSync       time.Time
	lastSize       int64
	dirty          map[string]bool
	deleted        map[string]bool
	dirtyFolders   map[string]bool
	deletedFolders map[string]bool
	loadedDefault  string // DefaultBrain como estava no load (detecta mudança externa)
	cfgDirty       bool   // nós mesmos mudamos a config desde o load

	blocksMu sync.Mutex
	blocks   *store.Store // depósito de blocos (aberto sob demanda)
}

// appendEntry insere entrada com proteção de leitores (painel/FUSE na mesma
// Store desde o fix da Store compartilhada) e marca mutação local pendente.
func (s *Store) appendEntry(e *Entry) {
	s.dataMu.Lock()
	if s.dirty == nil {
		s.dirty = map[string]bool{}
	}
	s.dirty[e.UID] = true
	s.Man.Entries = append(s.Man.Entries, e)
	s.dataMu.Unlock()
}

// setEntries substitui a lista inteira (remoções, migração); os UIDs que
// saíram ficam anotados como remoção local até o próximo Save sincronizar.
func (s *Store) setEntries(keep []*Entry) {
	s.dataMu.Lock()
	if s.deleted == nil {
		s.deleted = map[string]bool{}
	}
	cur := map[string]bool{}
	for _, e := range s.Man.Entries {
		cur[e.UID] = true
	}
	for _, e := range keep {
		delete(cur, e.UID)
	}
	for uid := range cur {
		s.deleted[uid] = true
	}
	s.Man.Entries = keep
	s.dataMu.Unlock()
}

// appendFolder cria pasta virtual com proteção de leitores.
func (s *Store) appendFolder(f string) {
	s.dataMu.Lock()
	if s.dirtyFolders == nil {
		s.dirtyFolders = map[string]bool{}
	}
	s.dirtyFolders[f] = true
	s.Man.Folders = append(s.Man.Folders, f)
	s.dataMu.Unlock()
}

// markDirtyUids marca entradas mutadas localmente (rename/move/replace).
func (s *Store) markDirty(uids ...string) {
	s.dataMu.Lock()
	if s.dirty == nil {
		s.dirty = map[string]bool{}
	}
	for _, u := range uids {
		s.dirty[u] = true
	}
	s.dataMu.Unlock()
}

// setFolders substitui a lista de pastas (renames/remoções); as pastas que
// saíram ficam anotadas para não ressuscitarem no merge.
func (s *Store) setFolders(folders []string) {
	s.dataMu.Lock()
	if s.deletedFolders == nil {
		s.deletedFolders = map[string]bool{}
	}
	if s.dirtyFolders == nil {
		s.dirtyFolders = map[string]bool{}
	}
	cur := map[string]bool{}
	for _, f := range s.Man.Folders {
		cur[f] = true
	}
	for _, f := range folders {
		delete(cur, f)
		delete(s.dirtyFolders, f)
	}
	for f := range cur {
		s.deletedFolders[f] = true
		delete(s.dirtyFolders, f)
	}
	s.Man.Folders = folders
	s.dataMu.Unlock()
}

// EntriesSnapshot devolve cópia da lista de entradas para leitura segura
// fora de qualquer lock (o painel serializa JSON enquanto o FUSE importa).
func (s *Store) EntriesSnapshot() []*Entry {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	return append([]*Entry(nil), s.Man.Entries...)
}

// FoldersSnapshot devolve cópia da lista de pastas.
func (s *Store) FoldersSnapshot() []string {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	return append([]string(nil), s.Man.Folders...)
}

func (s *Store) acquire() {
	s.sem <- struct{}{}
}

func (s *Store) release() {
	<-s.sem
}

const manifestVersion = 1

// DefaultRoot retorna o diretório padrão do cofre.
// Preferência: ~/Documentos/CromAr (visível e na partição do usuário);
// fallback: ~/.crom-ar (se ~/Documentos não existir).
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	docs := filepath.Join(home, "Documentos")
	if st, err := os.Stat(docs); err == nil && st.IsDir() {
		return filepath.Join(docs, "CromAr"), nil
	}
	return filepath.Join(home, ".crom-ar"), nil
}

// LegacyRoot é a raiz antiga (~/.crom-ar) para compatibilidade.
func LegacyRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".crom-ar"), nil
}

// LoadStore carrega config e manifest do root informado (cria se não existir).
func LoadStore(root string) (*Store, error) {
	cfgPath := filepath.Join(root, "config.json")
	var cfg Config
	if _, err := os.Stat(cfgPath); err == nil {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("config.json inválido: %w", err)
		}
	} else {
		cfg = Config{Root: root}
		cfg.VaultDir = filepath.Join(root, "vault")
		cfg.BrainsDir = filepath.Join(root, "brains")
		bin, err := FindBinary()
		if err != nil {
			return nil, err
		}
		cfg.CrompressorBin = bin
	}
	if cfg.Root == "" {
		cfg.Root = root
	}
	s := &Store{Cfg: &cfg, manPath: filepath.Join(root, "manifest.json"), timeNow: time.Now, sem: make(chan struct{}, 3)}
	if err := os.MkdirAll(cfg.VaultDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.BrainsDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.CrompressorBin); err != nil {
		bin, ferr := FindBinary()
		if ferr != nil {
			return nil, ferr
		}
		cfg.CrompressorBin = bin
	}
	if m, err := loadManifest(s.manPath); err == nil {
		s.Man = m
	} else {
		s.Man = &Manifest{Version: manifestVersion}
	}
	s.dirty = map[string]bool{}
	s.deleted = map[string]bool{}
	s.dirtyFolders = map[string]bool{}
	s.deletedFolders = map[string]bool{}
	if st, err := os.Stat(s.manPath); err == nil {
		s.lastSync = st.ModTime()
		s.lastSize = st.Size()
	}
	s.loadedDefault = cfg.DefaultBrain
	s.migrate()
	return s, nil
}

// migrate garante UID, pastas virtuais e campos novos nas entradas antigas.
func (s *Store) migrate() {
	changed := false
	if s.Man.Folders == nil {
		s.Man.Folders = []string{"/"}
		changed = true
	}
	if len(s.Man.Folders) == 0 {
		s.Man.Folders = []string{"/"}
		changed = true
	}
	if !s.HasFolder(ArchiveRoot) {
		s.Man.Folders = append(s.Man.Folders, ArchiveRoot)
		changed = true
	}
	sort.Strings(s.Man.Folders)
	// limpa lixeiras criadas por gerenciadores de arquivos dentro do cofre
	clean := s.Man.Folders[:0]
	for _, f := range s.Man.Folders {
		if !strings.Contains(f, "/.Trash-") && !strings.Contains(f, "/.Trash/") {
			clean = append(clean, f)
		}
	}
	if len(clean) != len(s.Man.Folders) {
		s.Man.Folders = clean
		changed = true
	}
	kept := s.EntriesSnapshot()[:0]
	for _, e := range s.Man.Entries {
		if strings.Contains(e.Path, "/.Trash-") || strings.Contains(e.Path, "/.Trash/") {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	s.setEntries(kept)
	for _, e := range s.EntriesSnapshot() {
		if e.UID == "" {
			e.UID = randomUID()
			changed = true
		}
		if e.Folder == "" {
			e.Folder = "/"
			changed = true
		}
		if e.Path == "" {
			e.Path = canonicalPath(e.Folder, e.RelPath)
			changed = true
		}
	}
	if changed {
		_ = s.Save()
	}
}

// canonicalPath deriva o caminho de exibição da pasta + relPath.
func canonicalPath(folder, relPath string) string {
	rel := filepath.ToSlash(relPath)
	dirRel := path.Dir(rel)
	if folder != "/" && dirRel != "." && strings.HasSuffix(folder, dirRel) {
		return strings.TrimSuffix(folder, dirRel) + "/" + rel
	}
	return path.Join(folder, rel)
}

func randomUID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// FindBinary localiza o binário crompressor.
func FindBinary() (string, error) {
	if p := os.Getenv("CROMPRESSOR_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "crompressor")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if root, err := DefaultRoot(); err == nil {
		p := filepath.Join(root, "bin", "crompressor")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	wd, _ := os.Getwd()
	p := filepath.Join(wd, "bin", "crompressor")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("binário crompressor não encontrado (use CROMPRESSOR_BIN=/caminho/crompressor ou coloque em bin/crompressor)")
}

// Save persiste config e manifest atomicamente. Se o manifest mudou em disco
// desde a última sincronização (outro processo gravou — CLI com o residente
// rodando), faz merge por UID antes de escrever: ninguém perde mutações.
// Antes de sobrescrever, rotaciona uma cópia do manifest (manifest.json.bak)
// — recuperação barata contra disco cheio/corrupção.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if st, err := os.Stat(s.manPath); err == nil {
		if !s.lastSync.IsZero() && (!st.ModTime().Equal(s.lastSync) || st.Size() != s.lastSize) {
			s.mergeExternal()
		}
	}
	if err := writeJSON(filepath.Join(s.Cfg.Root, "config.json"), s.Cfg); err != nil {
		return err
	}
	if b, err := os.ReadFile(s.manPath); err == nil {
		_ = os.WriteFile(s.manPath+".bak", b, 0o644)
	}
	if err := writeJSON(s.manPath, s.Man); err != nil {
		return err
	}
	if st, err := os.Stat(s.manPath); err == nil {
		s.saveMuHeldSync(st)
	}
	s.clearSyncState()
	return nil
}

// DefaultBrainPath retorna o codebook padrão do cofre, se existir.
// Respeita o nome configurado (DefaultBrain) e o legado vault.cromdb.
func (s *Store) DefaultBrainPath() string {
	if s.Cfg.DefaultBrain != "" {
		p := filepath.Join(s.Cfg.BrainsDir, s.Cfg.DefaultBrain+".cromdb")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p := filepath.Join(s.Cfg.BrainsDir, "vault.cromdb")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// DefaultBrainName retorna o nome do codebook padrão (para exibição).
func (s *Store) DefaultBrainName() string {
	if s.Cfg.DefaultBrain != "" {
		return s.Cfg.DefaultBrain
	}
	if _, err := os.Stat(filepath.Join(s.Cfg.BrainsDir, "vault.cromdb")); err == nil {
		return "vault"
	}
	return ""
}

// CromPath retorna o caminho absoluto do .crom da entrada.
func (s *Store) CromPath(e *Entry) string {
	return filepath.Join(s.Cfg.VaultDir, e.CromFile)
}

// HasContent informa se o conteúdo já está no cofre (.crom ou depósito).
func (s *Store) HasContent(hash string) bool {
	p := filepath.Join(s.Cfg.VaultDir, hash+".crom")
	if _, err := os.Stat(p); err == nil {
		return true
	}
	return s.hasBlocks(hash)
}

func loadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readConfig lê config.json para detectar mudanças externas (merge).
func readConfig(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// saveMuHeldSync atualiza os marcadores de sincronização (chamar só com
// saveMu tomado, logo após gravar o manifest).
func (s *Store) saveMuHeldSync(st os.FileInfo) {
	s.lastSync = st.ModTime()
	s.lastSize = st.Size()
}

// clearSyncState zera os marcadores após um Save bem-sucedido (o disco
// reflete o nosso estado; mutações futuras serão novas marcações).
func (s *Store) clearSyncState() {
	s.dataMu.Lock()
	s.dirty = map[string]bool{}
	s.deleted = map[string]bool{}
	s.dirtyFolders = map[string]bool{}
	s.deletedFolders = map[string]bool{}
	s.dataMu.Unlock()
	s.cfgDirty = false
	s.loadedDefault = s.Cfg.DefaultBrain
}

// mergeExternal incorpora no estado local o que outro processo gravou no
// manifest em disco. Autoridade do disco: entradas ausentes nele são
// removidas daqui; presentes são adotadas, exceto as com mutação local não
// sincronizada (dirty) — nessas a versão local vence. Roda com saveMu
// tomado e adquire dataMu (ordem fixa: s.mu → saveMu → dataMu).
func (s *Store) mergeExternal() {
	m, err := loadManifest(s.manPath)
	if err != nil {
		return // disco corrompido/ilegível: não sobrescrever com merge vazio
	}
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	diskByUID := map[string]bool{}
	merged := make([]*Entry, 0, len(m.Entries)+len(s.Man.Entries))
	for _, e := range m.Entries {
		diskByUID[e.UID] = true
		if s.deleted[e.UID] {
			continue // removido por nós — não ressuscitar
		}
		merged = append(merged, e)
	}
	for _, e := range s.Man.Entries {
		if !s.dirty[e.UID] {
			continue // disco é autoridade (inclui remoções externas)
		}
		if diskByUID[e.UID] {
			// presente nos dois: mutação local mais recente vence
			for i := range merged {
				if merged[i].UID == e.UID {
					merged[i] = e
					break
				}
			}
		} else {
			merged = append(merged, e) // adição nossa que o disco ainda não viu
		}
	}
	s.Man.Entries = merged

	// pastas: disco − deletedFolders ∪ dirtyFolders
	have := map[string]bool{}
	folders := make([]string, 0, len(m.Folders)+len(s.dirtyFolders))
	for _, f := range m.Folders {
		if !s.deletedFolders[f] && !have[f] {
			have[f] = true
			folders = append(folders, f)
		}
	}
	for _, f := range s.Man.Folders {
		if s.dirtyFolders[f] && !have[f] {
			have[f] = true
			folders = append(folders, f)
		}
	}
	sort.Strings(folders)
	s.Man.Folders = folders

	// config: adota DefaultBrain mudado externamente (ex.: CLI brain use)
	if diskCfg, err := readConfig(filepath.Join(s.Cfg.Root, "config.json")); err == nil {
		if diskCfg.DefaultBrain != s.loadedDefault && !s.cfgDirty {
			s.Cfg.DefaultBrain = diskCfg.DefaultBrain
		}
		s.loadedDefault = s.Cfg.DefaultBrain
	}
}

// SHA256File calcula o hash streaming de um arquivo.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, make([]byte, 1<<20))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Run executa o crompressor capturando stdout/stderr.
// Limita subprocessos simultâneos (cada um dispara 4 goroutines internas).
func (s *Store) Run(args ...string) (string, error) {
	s.acquire()
	defer s.release()
	cmd := exec.Command(s.Cfg.CrompressorBin, args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// RunPassthrough executa o crompressor compartilhando stdout/stderr do processo.
func (s *Store) RunPassthrough(args ...string) error {
	cmd := exec.Command(s.Cfg.CrompressorBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// ExpandHome expande ~ no início do caminho.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// ResolveRoot resolve a raiz do cofre: env CROM_AR_ROOT → ~/Documentos/CromAr
// (com fallback para o cofre legado ~/.crom-ar).
func ResolveRoot() string {
	if r := os.Getenv("CROM_AR_ROOT"); r != "" {
		return ExpandHome(r)
	}
	r, _ := DefaultRoot()
	if _, err := os.Stat(filepath.Join(r, "config.json")); err != nil {
		if legacy, err := LegacyRoot(); err == nil {
			if _, err2 := os.Stat(filepath.Join(legacy, "config.json")); err2 == nil {
				return legacy
			}
		}
	}
	return r
}

// BrainNameFromPath extrai o nome do codebook a partir de um caminho.
// Caminho vazio (bypass) devolve "" — não "." (filepath.Base de "" é ".").
func BrainNameFromPath(p string) string {
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(p), ".cromdb")
}
