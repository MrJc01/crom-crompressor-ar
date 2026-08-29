// Package bench testa estratégias de compressão do crompressor
// e recomenda a melhor forma de "triturar" cada tipo de dado.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

// Strategy é uma combinação de flags do crompressor pack.
type Strategy struct {
	Name string
	Args []string
}

// Row é o resultado de uma estratégia (agregado sobre os arquivos testados).
type Row struct {
	Strategy   string        `json:"strategy"`
	Files      int           `json:"files"`
	Original   int64         `json:"original_bytes"`
	Crom       int64         `json:"crom_bytes"`
	RatioPct   float64       `json:"ratio_pct"`
	PackTime   time.Duration `json:"pack_time"`
	UnpackTime time.Duration `json:"unpack_time"`
	Lossless   bool          `json:"lossless"`
	Err        string        `json:"error,omitempty"`
}

// Options do benchmark.
type Options struct {
	MaxFiles int    // máximo de arquivos quando a entrada é diretório
	Brain    string // codebook para comparar ("" = default do cofre / auto-treino)
	Encrypt  string
}

// AllStrategies retorna as estratégias padrão de teste.
func AllStrategies(brain string) []Strategy {
	var s []Strategy
	s = append(s, Strategy{Name: "plain", Args: nil})
	s = append(s, Strategy{Name: "cdc", Args: []string{"--cdc"}})
	s = append(s, Strategy{Name: "cdc+multi", Args: []string{"--cdc", "--multi-pass"}})
	if brain != "" {
		s = append(s, Strategy{Name: "brain", Args: []string{"-c", brain}})
		s = append(s, Strategy{Name: "cdc+brain", Args: []string{"--cdc", "-c", brain}})
		s = append(s, Strategy{Name: "cdc+brain+multi", Args: []string{"--cdc", "-c", brain, "--multi-pass"}})
	}
	return s
}

// Run executa o benchmark. Retorna as linhas e o caminho do codebook usado.
func Run(store *vault.Store, target string, opt Options) ([]Row, string, error) {
	target = vault.ExpandHome(target)
	info, err := os.Stat(target)
	if err != nil {
		return nil, "", err
	}

	var files []string
	if info.IsDir() {
		err = filepath.WalkDir(target, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.Type().IsRegular() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		sort.Strings(files)
		if opt.MaxFiles > 0 && len(files) > opt.MaxFiles {
			files = files[:opt.MaxFiles]
		}
	} else {
		files = []string{target}
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("nenhum arquivo para testar")
	}

	brain := opt.Brain
	if brain == "" {
		brain = store.DefaultBrainPath()
	}
	if brain == "" {
		// auto-treino temporário a partir dos próprios dados testados
		tmp, err := os.MkdirTemp("", "crom-ar-bench-*")
		if err != nil {
			return nil, "", err
		}
		defer os.RemoveAll(tmp)
		cb := filepath.Join(tmp, "bench.cromdb")
		if _, err := store.Run("train", "-i", filepath.Dir(files[0]), "-o", cb); err != nil {
			fmt.Fprintf(os.Stderr, "! auto-treino do codebook falhou (%v); testando sem codebook\n", err)
		} else {
			brain = cb
			fmt.Printf("✔ codebook temporário treinado para o bench: %s\n", cb)
		}
	}

	origHashes := make(map[string]string, len(files))
	origSizes := make(map[string]int64, len(files))
	for _, f := range files {
		h, sz, err := vault.SHA256File(f)
		if err != nil {
			return nil, "", err
		}
		origHashes[f] = h
		origSizes[f] = sz
	}

	tmp, err := os.MkdirTemp("", "crom-ar-bench-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp)

	var rows []Row
	for _, st := range AllStrategies(brain) {
		row := Row{Strategy: st.Name}
		ok := true
		for _, f := range files {
			row.Files++
			row.Original += origSizes[f]
			crom := filepath.Join(tmp, fmt.Sprintf("%s_%s.crom", st.Name, filepath.Base(f)))
			rest := filepath.Join(tmp, fmt.Sprintf("rest_%s_%s", st.Name, filepath.Base(f)))
			args := append([]string{"pack", "-i", f, "-o", crom}, st.Args...)
			if opt.Encrypt != "" {
				args = append(args, "--encrypt", opt.Encrypt)
			}
			t0 := time.Now()
			out, err := store.Run(args...)
			row.PackTime += time.Since(t0)
			if err != nil {
				ok = false
				row.Err = filepath.Base(f) + " (pack): " + lastLine(out)
				break
			}
			if fi, err := os.Stat(crom); err == nil {
				row.Crom += fi.Size()
			}
			uargs := []string{"unpack", "-i", crom, "-o", rest}
			if brain != "" {
				uargs = append(uargs, "-c", brain)
			}
			if opt.Encrypt != "" {
				uargs = append(uargs, "--encrypt", opt.Encrypt)
			}
			t1 := time.Now()
			out, err = store.Run(uargs...)
			row.UnpackTime += time.Since(t1)
			if err != nil {
				ok = false
				row.Err = filepath.Base(f) + " (unpack): " + lastLine(out)
				break
			}
			h, _, err := vault.SHA256File(rest)
			if err != nil || h != origHashes[f] {
				ok = false
				row.Err = filepath.Base(f) + ": hash divergente após unpack"
				break
			}
			os.Remove(crom)
			os.Remove(rest)
		}
		if row.Original > 0 {
			row.RatioPct = float64(row.Crom) / float64(row.Original) * 100
		}
		row.Lossless = ok && row.Err == ""
		rows = append(rows, row)
	}
	return rows, brain, nil
}

// Print imprime a tabela de resultados e a recomendação.
func Print(rows []Row) {
	fmt.Printf("\n%-22s %12s %12s %8s %10s %10s  %s\n",
		"ESTRATÉGIA", "ORIGINAL", "CROM", "RATIO", "PACK", "UNPACK", "LOSSLESS")
	fmt.Println(strings.Repeat("─", 92))
	var best *Row
	plainBroken := false
	for _, r := range rows {
		loss := "✔"
		if !r.Lossless {
			loss = "✘ " + r.Err
			if !strings.Contains(r.Strategy, "brain") && strings.Contains(r.Err, "unpack") {
				plainBroken = true
			}
		}
		fmt.Printf("%-22s %12s %12s %7.1f%% %9v %9v  %s\n",
			r.Strategy, vault.HumanBytes(r.Original), vault.HumanBytes(r.Crom),
			r.RatioPct, r.PackTime.Round(time.Millisecond), r.UnpackTime.Round(time.Millisecond), loss)
		if r.Lossless && r.Original > 0 && (best == nil || r.Crom < best.Crom) {
			b := r
			best = &b
		}
	}
	fmt.Println(strings.Repeat("─", 92))
	if best != nil {
		fmt.Printf("🏆 Melhor forma lossless: %s (%.1f%% do original, %v de pack)\n",
			best.Strategy, best.RatioPct, best.PackTime.Round(time.Millisecond))
	} else {
		fmt.Println("⚠ nenhuma estratégia foi lossless — verifique erros acima")
	}
	if plainBroken {
		fmt.Println("💡 pack SEM codebook falhou na verificação: no V24 o .crom sem codebook não é\n" +
			"   self-contained — sempre empacote com um codebook (o cofre crom-ar faz isso por padrão).")
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
