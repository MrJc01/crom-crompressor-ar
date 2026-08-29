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

// BrainStat resume a economia que um codebook está liberando (ou causando)
// sobre as entradas que foram empacotadas com ele.
type BrainStat struct {
	Name     string  `json:"name"`      // "" = empacotado sem codebook (bypass)
	Default  bool    `json:"default"`   // é o codebook padrão do cofre
	DBSize   int64   `json:"db_size"`   // custo do .cromdb em disco
	Members  int64   `json:"members"`   // entradas lógicas
	Unique   int64   `json:"unique"`    // conteúdos únicos (.crom próprios)
	Original int64   `json:"original"`  // bytes de origem (visão lógica, dedup incluso)
	Stored   int64   `json:"stored"`    // bytes .crom (conteúdos únicos)
	Saved    int64   `json:"saved"`     // original - stored (negativo = causando overhead)
	Ratio    float64 `json:"ratio_pct"` // stored/original
	AvgHit   float64 `json:"avg_hit"`   // hit rate médio dos chunks
}

// ComputeBrainStats agrega a economia por codebook a partir do manifest.
// O armazenado conta cada .crom uma única vez (um .crom pertence a exatamente
// um codebook — aquele usado no pack), então as linhas somam aos totais globais.
func (s *Store) ComputeBrainStats() []BrainStat {
	def := s.DefaultBrainName()
	dbSize := map[string]int64{}
	for _, b := range s.ListBrains() {
		dbSize[b.Name] = b.Size
	}
	order := []string{}
	byName := map[string]*BrainStat{}
	entries := s.EntriesSnapshot()
	get := func(name string) *BrainStat {
		if bs := byName[name]; bs != nil {
			return bs
		}
		bs := &BrainStat{Name: name, Default: name != "" && name == def, DBSize: dbSize[name]}
		byName[name] = bs
		order = append(order, name)
		return bs
	}
	seen := map[string]bool{}
	for _, e := range entries {
		bs := get(BrainNameFromPath(e.Codebook))
		bs.Members++
		bs.Original += e.Size
		bs.AvgHit += e.HitRate
		if !seen[e.CromFile] {
			seen[e.CromFile] = true
			bs.Stored += e.CromSize
			bs.Unique++
		}
	}
	out := make([]BrainStat, 0, len(order))
	for _, name := range order {
		bs := byName[name]
		bs.Saved = bs.Original - bs.Stored
		if bs.Original > 0 {
			bs.Ratio = float64(bs.Stored) / float64(bs.Original) * 100
		}
		if bs.Members > 0 {
			bs.AvgHit /= float64(bs.Members)
		}
		out = append(out, *bs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		if out[i].Saved != out[j].Saved {
			return out[i].Saved > out[j].Saved
		}
		return out[i].Name < out[j].Name
	})
	return out
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

// RemoveBrain apaga um codebook (recusa o padrão em uso e qualquer
// codebook que ainda tenha entradas empacotadas com ele — sem ele o
// unpack dessas entradas quebraria).
func (s *Store) RemoveBrain(name string) error {
	name = strings.TrimSuffix(name, ".cromdb")
	if name == s.DefaultBrainName() {
		return fmt.Errorf("%s é o codebook padrão — use 'brain use <outro>' antes", name)
	}
	if !s.BrainExists(name) {
		return fmt.Errorf("codebook não encontrado: %s", name)
	}
	if n := s.brainRefCount(name); n > 0 {
		return fmt.Errorf("%s é usado por %d entrada(s) — remova-as (crom-ar rm) ou repack antes de apagar o codebook", name, n)
	}
	return os.Remove(s.BrainPath(name))
}

// brainRefCount conta entradas empacotadas com o codebook informado.
func (s *Store) brainRefCount(name string) int {
	n := 0
	for _, e := range s.EntriesSnapshot() {
		if BrainNameFromPath(e.Codebook) == name {
			n++
		}
	}
	return n
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
