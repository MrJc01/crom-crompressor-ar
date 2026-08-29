// Package vault implementa o cofre local de arquivos .crom do crom-ar:
// manifest, config, dedup por SHA-256 e operações add/extract/rm/stats.
package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	CromFile   string    `json:"crom_file"` // nome do arquivo dentro de VaultDir
	CromSize   int64     `json:"crom_size"`
	Codebook   string    `json:"codebook"` // "" = empacotado sem codebook
	HitRate    float64   `json:"hit_rate"` // % de chunks que caíram no codebook
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
	Cfg      *Config
	Man      *Manifest
	manPath  string
	timeNow  func() time.Time
	mu       sync.Mutex
	sem      chan struct{}
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
	for _, e := range s.Man.Entries {
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

// Save persiste config e manifest atomicamente.
func (s *Store) Save() error {
	if err := writeJSON(filepath.Join(s.Cfg.Root, "config.json"), s.Cfg); err != nil {
		return err
	}
	return writeJSON(s.manPath, s.Man)
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

// HasContent informa se já existe .crom para o hash (dedup).
func (s *Store) HasContent(hash string) bool {
	p := filepath.Join(s.Cfg.VaultDir, hash+".crom")
	_, err := os.Stat(p)
	return err == nil
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
func BrainNameFromPath(p string) string {
	return strings.TrimSuffix(filepath.Base(p), ".cromdb")
}
