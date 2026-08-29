// Package bundle implementa o pacote portátil .cromar:
// um zip auto-contido com os membros do pacote (comprimidos), o codebook
// e os metadados — exportável, montável (read-only) e importável.
package bundle

import (
	"archive/tar"
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MrJc01/crompressor/pkg/cromlib"

	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

// Meta descreve o conteúdo do pacote .cromar.
type Meta struct {
	Group   string    `json:"group"`
	Brain   string    `json:"brain"`
	Mode    string    `json:"mode"` // "full" (lossless) ou "edge" (esqueleto lossy)
	Created time.Time `json:"created"`
	Members []Member  `json:"members"`
}

// Member é um arquivo do pacote.
type Member struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

// Export empacota um grupo num .cromar portátil (membros + codebook).
// Fluxo: descomprime membros → tar → crompressor pack → verifica roundtrip
// → zip (meta.json + brain.cromdb + content.crom).
func Export(st *vault.Store, group, outPath, brainName string, edge bool) (int, error) {
	var members []*vault.Entry
	for _, e := range st.Man.Entries {
		if e.Group == group {
			members = append(members, e)
		}
	}
	if len(members) == 0 {
		return 0, fmt.Errorf("pacote não encontrado ou vazio: %s", group)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].RelPath < members[j].RelPath })

	brain := brainName
	if brain == "" {
		for _, e := range members {
			if e.Codebook != "" {
				brain = vault.BrainNameFromPath(e.Codebook)
				break
			}
		}
	}
	if brain == "" {
		brain = st.DefaultBrainName()
	}
	if brain == "" {
		return 0, fmt.Errorf("sem codebook — rode 'crom-ar brain train' antes de exportar")
	}
	brainPath := st.BrainPath(brain)
	if _, err := os.Stat(brainPath); err != nil {
		return 0, fmt.Errorf("codebook %s não encontrado no cofre", brain)
	}

	tmp, err := os.MkdirTemp("", "crom-ar-export-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)

	// 1) descomprime cada membro (conteúdo real para o tar)
	dirA := filepath.Join(tmp, "a")
	if err := os.MkdirAll(dirA, 0o700); err != nil {
		return 0, err
	}
	for _, e := range members {
		dst := filepath.Join(dirA, filepath.FromSlash(e.RelPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return 0, err
		}
		if _, err := st.Run("unpack", "-i", st.CromPath(e), "-o", dst, "-c", brainPath); err != nil {
			return 0, fmt.Errorf("unpack %s: %v", e.RelPath, err)
		}
	}

	// 2) tar
	tarPath := filepath.Join(tmp, "bundle.tar")
	if err := writeTar(dirA, members, tarPath); err != nil {
		return 0, err
	}

	// 3) pack: lossless (vault) ou esqueleto lossy (edge, sem deltas)
	contentCrom := filepath.Join(tmp, "content.crom")
	mode := "full"
	if edge {
		mode = "edge"
		opts := cromlib.DefaultPackOptions()
		opts.Mode = "edge"
		opts.UseCDC = true
		if _, err := cromlib.Pack(tarPath, contentCrom, brainPath, opts); err != nil {
			return 0, fmt.Errorf("pack esqueleto: %v", err)
		}
	} else {
		if _, err := st.Run("pack", "-i", tarPath, "-o", contentCrom, "--cdc", "-c", brainPath); err != nil {
			return 0, fmt.Errorf("pack do pacote: %v", err)
		}
	}

	// 4) verificação lossless do roundtrip completo (só modo full)
	if mode == "full" {
		dirB := filepath.Join(tmp, "b")
		if err := os.MkdirAll(dirB, 0o700); err != nil {
			return 0, err
		}
		tar2 := filepath.Join(tmp, "bundle2.tar")
		if _, err := st.Run("unpack", "-i", contentCrom, "-o", tar2, "-c", brainPath); err != nil {
			return 0, fmt.Errorf("verificação: %v", err)
		}
		if err := untar(tar2, dirB); err != nil {
			return 0, err
		}
		for _, e := range members {
			h, _, err := vault.SHA256File(filepath.Join(dirB, filepath.FromSlash(e.RelPath)))
			if err != nil {
				return 0, err
			}
			if h != e.ID {
				return 0, fmt.Errorf("fidelidade falhou para %s", e.RelPath)
			}
		}
	}

	// 5) zip final
	outPath = vault.ExpandHome(outPath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 0, err
	}
	tmpZip := outPath + ".tmp"
	zf, err := os.Create(tmpZip)
	if err != nil {
		return 0, err
	}
	defer zf.Close()
	w := zip.NewWriter(zf)
	if err := writeZipFile(w, "meta.json", marshalMeta(Meta{
		Group: group, Brain: brain, Mode: mode, Created: time.Now(), Members: memberList(members),
	})); err != nil {
		return 0, err
	}
	if err := copyIntoZip(w, "brain.cromdb", brainPath); err != nil {
		return 0, err
	}
	if err := copyIntoZip(w, "content.crom", contentCrom); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	if err := zf.Close(); err != nil {
		return 0, err
	}
	return len(members), os.Rename(tmpZip, outPath)
}

// Open abre um .cromar e extrai para um diretório de cache.
// Retorna os metadados e o diretório com os membros originais.
func Open(st *vault.Store, bundlePath string) (*Meta, string, error) {
	zr, err := zip.OpenReader(vault.ExpandHome(bundlePath))
	if err != nil {
		return nil, "", err
	}
	defer zr.Close()
	cache, err := os.MkdirTemp("", "crom-ar-cd-*")
	if err != nil {
		return nil, "", err
	}
	var meta Meta
	var brainData, contentData []byte
	for _, f := range zr.File {
		switch f.Name {
		case "meta.json":
			rc, e := f.Open()
			if e != nil {
				return nil, "", e
			}
			err = json.NewDecoder(rc).Decode(&meta)
			rc.Close()
			if err != nil {
				return nil, "", err
			}
		case "brain.cromdb":
			rc, e := f.Open()
			if e != nil {
				return nil, "", e
			}
			brainData, _ = io.ReadAll(rc)
			rc.Close()
		case "content.crom":
			rc, e := f.Open()
			if e != nil {
				return nil, "", e
			}
			contentData, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if contentData == nil {
		return nil, "", fmt.Errorf("pacote sem content.crom")
	}
	brainPath := filepath.Join(cache, "brain.cromdb")
	if brainData != nil {
		if err := os.WriteFile(brainPath, brainData, 0o644); err != nil {
			return nil, "", err
		}
	}
	contentPath := filepath.Join(cache, "content.crom")
	if err := os.WriteFile(contentPath, contentData, 0o644); err != nil {
		return nil, "", err
	}
	srcDir := filepath.Join(cache, "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		return nil, "", err
	}
	uopts := cromlib.DefaultUnpackOptions()
	uopts.Fuzziness = 0.0001 // tolera bundles edge (sem hash-check interno)
	if err := cromlib.Unpack(contentPath, filepath.Join(cache, "bundle.tar"), brainPath, uopts); err != nil {
		return nil, "", fmt.Errorf("unpack do pacote: %v", err)
	}
	if err := untar(filepath.Join(cache, "bundle.tar"), srcDir); err != nil {
		return nil, "", err
	}
	return &meta, srcDir, nil
}

// Import reintroduz um .cromar no cofre (instala o codebook e os membros).
func Import(st *vault.Store, bundlePath, intoFolder string) (int, error) {
	meta, srcDir, err := Open(st, bundlePath)
	if err != nil {
		return 0, err
	}
	// codebook
	if _, err := st.AddBrain(filepath.Join(filepath.Dir(srcDir), "brain.cromdb"), meta.Brain); err != nil {
		return 0, err
	}
	group := meta.Group
	if intoFolder != "" {
		group = intoFolder
	}
	count := 0
	for _, m := range meta.Members {
		src := filepath.Join(srcDir, filepath.FromSlash(m.Name))
		if _, err := st.ImportFile(vault.AddFileOpts{
			Src: src, Folder: vault.ArchiveRoot + "/" + group, Name: filepath.Base(m.Name),
			Rel: m.Name, Group: group, IsDir: true,
			Opt: vault.AddOptions{CDC: true, Brain: st.BrainPath(meta.Brain)},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "! %s: %v\n", m.Name, err)
			continue
		}
		count++
	}
	if err := st.CreateFolder(vault.ArchiveRoot + "/" + group); err != nil {
		return count, nil
	}
	return count, nil
}

// ExtractFile extrai os membros do .cromar direto para o HD (sem cofre).
// Usado pela action do Nemo ("Extrair aqui").
func ExtractFile(bundlePath, destDir string) (int, error) {
	bundlePath = vault.ExpandHome(bundlePath)
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	var meta Meta
	for _, f := range zr.File {
		if f.Name != "meta.json" {
			continue
		}
		rc, e := f.Open()
		if e != nil {
			return 0, e
		}
		err = json.NewDecoder(rc).Decode(&meta)
		rc.Close()
		if err != nil {
			return 0, err
		}
	}
	st, err := openStoreForBundle()
	if err != nil {
		return 0, err
	}
	_, srcDir, err := Open(st, bundlePath)
	if err != nil {
		return 0, err
	}
	dest := vault.ExpandHome(destDir)
	target := filepath.Join(dest, meta.Group)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return 0, err
	}
	return copyTree(srcDir, target)
}

func copyTree(src, dst string) (int, error) {
	n := 0
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// openStoreForBundle abre o cofre padrão (uso interno do ExtractFile).
func openStoreForBundle() (*vault.Store, error) {
	return vault.LoadStore(vault.ResolveRoot())
}

func writeTar(srcDir string, members []*vault.Entry, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()
	for _, e := range members {
		src := filepath.Join(srcDir, filepath.FromSlash(e.RelPath))
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = e.RelPath
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		file, err := os.Open(src)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, file); err != nil {
			file.Close()
			return err
		}
		file.Close()
	}
	return nil
}

func untar(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		out := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(filepath.Clean(out), filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("caminho suspeito no tar: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			outF, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outF, tr); err != nil {
				outF.Close()
				return err
			}
			outF.Close()
		}
	}
}

func memberList(entries []*vault.Entry) []Member {
	var out []Member
	for _, e := range entries {
		out = append(out, Member{Name: e.RelPath, Size: e.Size, Sha256: e.ID})
	}
	return out
}

func marshalMeta(m Meta) []byte {
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

func writeZipFile(w *zip.Writer, name string, data []byte) error {
	f, err := w.Create(name)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func copyIntoZip(w *zip.Writer, name, srcPath string) error {
	f, err := w.Create(name)
	if err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(f, src)
	return err
}


