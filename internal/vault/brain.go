// Codebooks nomeados do cofre: list/add/use/remove/export/train.
// Um .cromdb é um arquivo portátil — compartilhável entre cofres e pessoas.
package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Brain é um codebook do cofre.
type Brain struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Default bool   `json:"default"`
}

// ListBrains lista os codebooks do cofre.
func (s *Store) ListBrains() []Brain {
	var out []Brain
	files, _ := filepath.Glob(filepath.Join(s.Cfg.BrainsDir, "*.cromdb"))
	sort.Strings(files)
	def := s.DefaultBrainName()
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".cromdb")
		var sz int64
		if st, err := os.Stat(f); err == nil {
			sz = st.Size()
		}
		out = append(out, Brain{Name: name, Path: f, Size: sz, Default: name == def})
	}
	return out
}

// BrainPath resolve o caminho de um codebook pelo nome.
func (s *Store) BrainPath(name string) string {
	name = strings.TrimSuffix(name, ".cromdb")
	return filepath.Join(s.Cfg.BrainsDir, name+".cromdb")
}

// BrainExists informa se o codebook existe.
func (s *Store) BrainExists(name string) bool {
	_, err := os.Stat(s.BrainPath(name))
	return err == nil
}

// AddBrain importa um .cromdb para o cofre (nome = base do arquivo ou informado).
func (s *Store) AddBrain(src, name string) (string, error) {
	src = ExpandHome(src)
	if _, err := os.Stat(src); err != nil {
		return "", err
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	}
	name = strings.TrimSuffix(name, ".cromdb")
	dst := s.BrainPath(name)
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	// primeiro brain do cofre vira o default
	if s.DefaultBrainName() == "" || s.DefaultBrainName() == "" {
		s.Cfg.DefaultBrain = name
		if err := s.Save(); err != nil {
			return name, err
		}
	}
	return name, nil
}

// UseBrain define o codebook padrão das próximas importações.
func (s *Store) UseBrain(name string) error {
	if !s.BrainExists(name) {
		return fmt.Errorf("codebook não encontrado: %s", name)
	}
	s.Cfg.DefaultBrain = strings.TrimSuffix(name, ".cromdb")
	return s.Save()
}

// RemoveBrain apaga um codebook (recusa o padrão em uso).
func (s *Store) RemoveBrain(name string) error {
	name = strings.TrimSuffix(name, ".cromdb")
	if name == s.DefaultBrainName() {
		return fmt.Errorf("%s é o codebook padrão — use 'brain use <outro>' antes", name)
	}
	if !s.BrainExists(name) {
		return fmt.Errorf("codebook não encontrado: %s", name)
	}
	return os.Remove(s.BrainPath(name))
}

// ExportBrain copia um codebook para fora do cofre (compartilhamento).
func (s *Store) ExportBrain(name, outPath string) error {
	name = strings.TrimSuffix(name, ".cromdb")
	if !s.BrainExists(name) {
		return fmt.Errorf("codebook não encontrado: %s", name)
	}
	data, err := os.ReadFile(s.BrainPath(name))
	if err != nil {
		return err
	}
	outPath = ExpandHome(outPath)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return err
	}
	return nil
}

// TrainNamedBrain treina um codebook nomeado a partir de um diretório.
func (s *Store) TrainNamedBrain(name, srcDir string, size int) (string, error) {
	out := s.BrainPath(name)
	args := []string{"train", "-i", ExpandHome(srcDir), "-o", out}
	if size > 0 {
		args = append(args, "--size", fmt.Sprint(size))
	}
	if _, err := s.Run(args...); err != nil {
		return "", err
	}
	return out, nil
}
