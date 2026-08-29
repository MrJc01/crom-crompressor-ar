// Package fusefs monta o cofre crom-ar como um filesystem real:
// leitura descomprime on-demand, escrita comprime com dedup, pastas do manifest
// viram diretórios nativos (drag & drop no gerenciador de arquivos).
package fusefs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

// Options controla o comportamento do filesystem montado.
type Options struct {
	SpoolDir string // onde guardar arquivos descomprimidos (cache de leitura)
	Brain    string // codebook para escrita ("" = default do cofre)
	NoVerify bool   // pular verificação lossless ao importar
}

// VaultFS expõe o cofre via FUSE (API pathfs).
type VaultFS struct {
	pathfs.FileSystem // defaults (não-nil); métodos sobrescritos abaixo
	st                *vault.Store
	opt               Options
	unpackMu          sync.Mutex
	unpacks           map[string]*sync.Mutex

	// arquivos criados/abertos para escrita e ainda não importados —
	// o GetAttr precisa deles para o reply do CREATE não sair com attr zero
	pendMu  sync.Mutex
	pending map[string]*pendingFile
}

type pendingFile struct {
	size         int64
	mtime        time.Time
	tmp          string
	orphan       bool // desvinculado antes do close: dados devem sumir
	handleClosed bool // último fd fechado: pronto para importar
	imported     bool // já importado (aguardando limpeza)
	importing    bool
	opts         *vault.AddFileOpts // metadados p/ importar (persistidos em sidecar .meta)
}

// startJanitor importa escritas concluídas em background — o kernel pode
// entregar writes DEPOIS do FLUSH (bash faz open→dup2→close; cada close
// dispara FLUSH), então RELEASE é o único ponto garantido de conclusão.
func (v *VaultFS) startJanitor() {
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			for name, p := range v.pendingSnapshot() {
				if p.handleClosed && !p.imported && !p.orphan {
					if v.importPending(name, p) {
						v.dropPending(name)
					}
				}
			}
		}
	}()
}

func (v *VaultFS) pendingSnapshot() map[string]*pendingFile {
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	cp := make(map[string]*pendingFile, len(v.pending))
	for k, p := range v.pending {
		cp[k] = p
	}
	return cp
}

// tryImportPending importa sob demanda (leitores esperam o pack terminar).
func (v *VaultFS) tryImportPending(name string) {
	if p := v.getPending(name); p != nil && p.handleClosed && !p.imported && !p.orphan {
		if v.importPending(name, p) {
			v.dropPending(name)
		}
	}
}

// importPending executa a importação de um pending concluído.
func (v *VaultFS) importPending(name string, p *pendingFile) bool {
	v.pendMu.Lock()
	if p.importing {
		v.pendMu.Unlock()
		return false
	}
	p.importing = true
	v.pendMu.Unlock()

	if _, err := os.Stat(p.tmp); err != nil {
		v.dropPending(name)
		return false
	}
	o := *p.opts
	o.Src = p.tmp
	e, err := v.st.ImportFile(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "! falha ao importar %s: %v\n", name, err)
		v.pendMu.Lock()
		p.importing = false
		v.pendMu.Unlock()
		return false
	}
	_ = os.Rename(p.tmp, v.spoolPath(e.ID))
	v.pendMu.Lock()
	p.imported = true
	v.pendMu.Unlock()
	return true
}

// New cria o filesystem do cofre.
func New(st *vault.Store, opt Options) *VaultFS {
	if opt.SpoolDir == "" {
		opt.SpoolDir = filepath.Join(st.Cfg.Root, "spool")
	}
	_ = os.MkdirAll(opt.SpoolDir, 0o700)
	vf := &VaultFS{
		FileSystem: pathfs.NewDefaultFileSystem(),
		st:         st,
		opt:        opt,
		unpacks:    map[string]*sync.Mutex{},
		pending:    map[string]*pendingFile{},
	}
	vf.recoverPending()
	vf.cleanOldSpool()
	vf.startJanitor()
	return vf
}

func (v *VaultFS) String() string { return "crom-ar" }

// recoverPending reimporta escritas órfãs de um crash anterior:
// cada tmp com sidecar .meta tem os metadados completos de importação.
func (v *VaultFS) recoverPending() {
	metas, err := filepath.Glob(filepath.Join(v.opt.SpoolDir, "tmp-*.meta"))
	if err != nil {
		return
	}
	for _, mp := range metas {
		b, err := os.ReadFile(mp)
		if err != nil {
			continue
		}
		var o vault.AddFileOpts
		if json.Unmarshal(b, &o) != nil {
			os.Remove(mp)
			continue
		}
		tmp := strings.TrimSuffix(mp, ".meta")
		if _, err := os.Stat(tmp); err != nil {
			os.Remove(mp)
			continue
		}
		if _, err := v.st.ImportFile(o); err != nil {
			fmt.Fprintf(os.Stderr, "! recuperação de %s falhou: %v\n", o.Name, err)
			continue
		}
		os.Remove(tmp)
		os.Remove(mp)
		fmt.Printf("✔ recuperado do crash: %s\n", o.Name)
	}
}

func (v *VaultFS) writeMeta(p *pendingFile) {
	if p.opts == nil || p.tmp == "" {
		return
	}
	b, err := json.Marshal(p.opts)
	if err != nil {
		return
	}
	_ = os.WriteFile(p.tmp+".meta", b, 0o600)
}

func (v *VaultFS) removeMeta(tmp string) {
	if tmp != "" {
		os.Remove(tmp + ".meta")
	}
}

// buildImportOpts monta os metadados de importação do caminho informado.
// Com entrada existente (edição), preserva a associação de pacote/grupo.
func (v *VaultFS) buildImportOpts(name string, e *vault.Entry) *vault.AddFileOpts {
	base := path.Base(name)
	opt := vault.AddOptions{CDC: true, Brain: v.opt.Brain, NoVerify: v.opt.NoVerify}
	if e != nil {
		return &vault.AddFileOpts{
			Folder: e.Folder, Name: base, Rel: e.RelPath, Group: e.Group, IsDir: e.IsDir, Opt: opt,
		}
	}
	if isArch, folder, group, rel := vault.ParseArchivePath(name); isArch {
		o := &vault.AddFileOpts{Folder: folder, Name: base, Group: group, Rel: rel, Opt: opt}
		if rel == group {
			// solto direto em /arquivos: pacote de arquivo único
			o.Folder = vault.ArchiveRoot
			o.IsDir = false
		} else {
			o.IsDir = true
		}
		return o
	}
	return &vault.AddFileOpts{
		Folder: vault.NormalizeFolderInput(path.Dir(name)), Name: base, Opt: opt,
	}
}

// registerPending registra o handle de escrita e persiste o sidecar .meta
// (se o processo morrer antes do close, o mount seguinte reimporta).
func (v *VaultFS) registerPending(name, tmp string, opts *vault.AddFileOpts) {
	v.pendMu.Lock()
	v.pending[name] = &pendingFile{mtime: time.Now(), tmp: tmp, opts: opts}
	v.pendMu.Unlock()
	v.writeMeta(v.pendingMap()[name])
}

func (v *VaultFS) pendingMap() map[string]*pendingFile {
	return v.pending
}

func (v *VaultFS) setPendingSize(name string, size int64) {
	v.pendMu.Lock()
	if p := v.pending[name]; p != nil {
		p.size = size
		p.mtime = time.Now()
	}
	v.pendMu.Unlock()
}

func (v *VaultFS) getPending(name string) *pendingFile {
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	return v.pending[name]
}

func (v *VaultFS) dropPending(name string) {
	v.pendMu.Lock()
	p := v.pending[name]
	delete(v.pending, name)
	v.pendMu.Unlock()
	if p != nil {
		v.removeMeta(p.tmp)
	}
}

func (v *VaultFS) brainFor(e *vault.Entry) string {
	if e.Codebook != "" {
		return e.Codebook
	}
	return v.st.DefaultBrainPath()
}

func (v *VaultFS) cleanOldSpool() {
	cutoff := time.Now().Add(-24 * time.Hour)
	entries, err := os.ReadDir(v.opt.SpoolDir)
	if err != nil {
		return
	}
	for _, d := range entries {
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(v.opt.SpoolDir, d.Name()))
		}
	}
}

func (v *VaultFS) spoolPath(hash string) string {
	return filepath.Join(v.opt.SpoolDir, hash)
}

// unpackOnce garante o spool descomprimido do conteúdo (uma única vez por hash).
func (v *VaultFS) unpackOnce(hash, cromFile, brain string) (string, error) {
	v.unpackMu.Lock()
	m := v.unpacks[hash]
	if m == nil {
		m = &sync.Mutex{}
		v.unpacks[hash] = m
	}
	v.unpackMu.Unlock()

	m.Lock()
	defer m.Unlock()
	sp := v.spoolPath(hash)
	if _, err := os.Stat(sp); err == nil {
		return sp, nil
	}
	crom := filepath.Join(v.st.Cfg.VaultDir, cromFile)
	if brain == "" {
		return "", fmt.Errorf("nenhum codebook disponível para %s", cromFile)
	}
	out, err := v.st.Run("unpack", "-i", crom, "-o", sp, "-c", brain)
	if err != nil {
		os.Remove(sp)
		return "", fmt.Errorf("unpack: %v: %s", err, lastLine(out))
	}
	return sp, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func cleanPath(name string) string {
	return "/" + strings.Trim(path.Clean("/"+name), "/")
}

// lookup decide o que existe no caminho: dir (pasta virtual ou sintetizada) ou arquivo.
func (v *VaultFS) lookup(name string) (dir bool, entry *vault.Entry) {
	if name == "" || name == "/" {
		return true, nil
	}
	if v.st.HasFolder(vault.NormalizeFolderInput(name)) {
		return true, nil
	}
	if e := v.st.EntryByPath(name); e != nil {
		return false, e
	}
	prefix := strings.TrimSuffix(name, "/") + "/"
	for _, e := range v.st.Man.Entries {
		if strings.HasPrefix(e.Path, prefix) {
			return true, nil
		}
	}
	return false, nil
}

// GetAttr apresenta metadados nativos. Escrita recém-fechada e ainda não
// importada é importada aqui (leitor espera o pack terminar).
func (v *VaultFS) GetAttr(name string, ctx *fuse.Context) (*fuse.Attr, fuse.Status) {
	name = cleanPath(name)
	v.tryImportPending(name)
	out := &fuse.Attr{}
	dir, e := v.lookup(name)
	out.Owner = fuse.Owner{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	if dir {
		out.Mode = fuse.S_IFDIR | 0o755
		out.Size = 4096
		out.Nlink = 2
		return out, fuse.OK
	}
	if e != nil {
		out.Mode = fuse.S_IFREG | 0o644
		out.Size = uint64(e.Size)
		out.Nlink = 1
		out.Atime = uint64(e.PackedAt.Unix())
		out.Mtime = out.Atime
		out.Ctime = out.Atime
		return out, fuse.OK
	}
	if p := v.getPending(name); p != nil && !p.orphan {
		out.Mode = fuse.S_IFREG | 0o644
		out.Size = uint64(p.size)
		out.Nlink = 1
		out.Atime = uint64(p.mtime.Unix())
		out.Mtime = out.Atime
		out.Ctime = out.Atime
		return out, fuse.OK
	}
	return nil, fuse.ENOENT
}

// cromFile é um arquivo aberto: leitura do spool, escrita em tmp (dirty) e
// importação (pack+dedup) no Flush.
type cromFile struct {
	nodefs.File
	fs       *VaultFS
	name     string
	e        *vault.Entry
	sp       string
	tmp      string
	f        *os.File
	dirty    bool
	imported bool
}

func (c *cromFile) String() string { return "crom-ar:" + c.name }

func (c *cromFile) Read(dest []byte, off int64) (fuse.ReadResult, fuse.Status) {
	src := c.sp
	if c.dirty && c.tmp != "" {
		src = c.tmp
	}
	fin, err := os.Open(src)
	if err != nil {
		return nil, fuse.EIO
	}
	defer fin.Close()
	n, err := fin.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(dest[:n]), fuse.OK
}

func (c *cromFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	if c.tmp == "" || c.f == nil {
		return 0, fuse.EIO
	}
	n, err := c.f.WriteAt(data, off)
	if err != nil {
		return 0, fuse.EIO
	}
	if st, err := c.f.Stat(); err == nil {
		c.fs.setPendingSize(c.name, st.Size())
	}
	c.dirty = true
	return uint32(n), fuse.OK
}

func (c *cromFile) Flush() fuse.Status {
	if c.f != nil {
		if err := c.f.Sync(); err != nil {
			return fuse.EIO
		}
	}
	return fuse.OK
}

// Release finaliza o handle: marca pronto para importação (janitor/leitor
// importam depois). O kernel pode entregar writes depois do último FLUSH
// (cada close dispara FLUSH; bash faz open→dup2→close), então RELEASE é
// o único ponto garantido de "não vêm mais dados".
func (c *cromFile) Release() {
	if c.f != nil {
		c.f.Close()
	}
	if p := c.fs.getPending(c.name); p != nil {
		if st, err := os.Stat(c.tmp); err == nil {
			p.size = st.Size()
		}
		p.handleClosed = true
	}
}

func (c *cromFile) Truncate(size uint64) fuse.Status {
	if c.tmp == "" || c.f == nil {
		return fuse.EIO
	}
	if err := os.Truncate(c.tmp, int64(size)); err != nil {
		return fuse.EIO
	}
	c.dirty = true
	return fuse.OK
}

func (c *cromFile) GetAttr(out *fuse.Attr) fuse.Status {
	var size int64
	if c.tmp != "" && c.dirty {
		if st, err := os.Stat(c.tmp); err == nil {
			size = st.Size()
		}
	} else if st, err := os.Stat(c.sp); err == nil {
		size = st.Size()
	}
	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(size)
	out.Owner = fuse.Owner{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	return fuse.OK
}

// Open abre para leitura (descomprime on-demand) ou edição (cópia para tmp).
func (v *VaultFS) Open(name string, flags uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	name = cleanPath(name)
	v.tryImportPending(name)
	_, e := v.lookup(name)
	if e == nil {
		return nil, fuse.ENOENT
	}
	acc := flags & syscall.O_ACCMODE
	writeMode := acc != syscall.O_RDONLY

	sp, err := v.unpackOnce(e.ID, e.CromFile, v.brainFor(e))
	if err != nil {
		fmt.Fprintf(os.Stderr, "! %s: %v\n", name, err)
		return nil, fuse.EIO
	}

	c := &cromFile{File: nodefs.NewDefaultFile(), fs: v, name: name, e: e, sp: sp}
	if writeMode {
		tmp, err := os.CreateTemp(v.opt.SpoolDir, "tmp-*")
		if err != nil {
			return nil, fuse.EIO
		}
		src, err := os.Open(sp)
		if err != nil {
			tmp.Close()
			return nil, fuse.EIO
		}
		defer src.Close()
		if _, err := io.Copy(tmp, src); err != nil {
			tmp.Close()
			return nil, fuse.EIO
		}
		c.tmp = tmp.Name()
		c.f = tmp
		// O_WRONLY edita de cara; O_RDWR só marca dirty na primeira Write
		c.dirty = acc == syscall.O_WRONLY
		v.registerPending(name, c.tmp, v.buildImportOpts(name, e))
	}
	return &nodefs.WithFlags{File: c, FuseFlags: fuse.FOPEN_KEEP_CACHE, Description: "crom-ar"}, fuse.OK
}

// Create cria novo arquivo: grava em tmp e importa no Flush.
func (v *VaultFS) Create(name string, flags uint32, mode uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	name = cleanPath(name)
	if dir := vault.NormalizeFolderInput(path.Dir(name)); !v.st.HasFolder(dir) {
		return nil, fuse.ENOENT
	}
	tmp, err := os.CreateTemp(v.opt.SpoolDir, "tmp-*")
	if err != nil {
		return nil, fuse.EIO
	}
	v.registerPending(name, tmp.Name(), v.buildImportOpts(name, nil))
	// novo arquivo precisa ser importado no close mesmo sem writes (0-byte válido)
	c := &cromFile{File: nodefs.NewDefaultFile(), fs: v, name: name, tmp: tmp.Name(), f: tmp, dirty: true}
	return &nodefs.WithFlags{File: c, FuseFlags: fuse.FOPEN_KEEP_CACHE, Description: "crom-ar"}, fuse.OK
}

// Truncate aplica truncamento persistente (arquivo fechado).
func (v *VaultFS) Truncate(name string, size uint64, ctx *fuse.Context) fuse.Status {
	name = cleanPath(name)
	_, e := v.lookup(name)
	if e == nil {
		return fuse.ENOENT
	}
	sp, err := v.unpackOnce(e.ID, e.CromFile, v.brainFor(e))
	if err != nil {
		return fuse.EIO
	}
	if err := os.Truncate(sp, int64(size)); err != nil {
		return fuse.EIO
	}
	o := *v.buildImportOpts(name, e)
	o.Src = sp
	if _, err := v.st.ImportFile(o); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Unlink remove a entrada lógica (o .crom só sai se ficar órfão).
func (v *VaultFS) Unlink(name string, ctx *fuse.Context) fuse.Status {
	name = cleanPath(name)
	// arquivo em voo: marca órfão e remove tmp (Release não importará)
	if p := v.getPending(name); p != nil {
		p.orphan = true
		os.Remove(p.tmp)
		v.removeMeta(p.tmp)
		return fuse.OK
	}
	if _, err := v.st.RemoveByPath(name); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Mkdir cria pasta virtual.
func (v *VaultFS) Mkdir(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	if err := v.st.CreateFolder(name); err != nil {
		return fuse.Status(syscall.EEXIST)
	}
	return fuse.OK
}

// importPendingUnder importa sincronamente todas as pendências sob um
// caminho (usado antes de renomear/remover pastas — estado "settled").
func (v *VaultFS) importPendingUnder(dir string) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for name, p := range v.pendingSnapshot() {
		if name != dir && !strings.HasPrefix(name, prefix) {
			continue
		}
		if p.orphan {
			v.dropPending(name)
			continue
		}
		if !p.imported {
			v.importPending(name, p)
		}
		v.dropPending(name)
	}
}

// Rmdir remove pasta virtual. Pacote (/arquivos/<nome>) só sai se vazio.
func (v *VaultFS) Rmdir(name string, ctx *fuse.Context) fuse.Status {
	name = cleanPath(name)
	v.importPendingUnder(name)
	if isArch, _, group, _ := vault.ParseArchivePath(name); isArch && group != "" {
		if v.st.GroupHasEntries(group) {
			return fuse.Status(syscall.ENOTEMPTY)
		}
		if err := v.st.DeleteFolder(name); err != nil {
			return fuse.EINVAL
		}
		return fuse.OK
	}
	if err := v.st.DeleteFolder(name); err != nil {
		return fuse.EINVAL
	}
	return fuse.OK
}

// Rename renomeia pasta (RenameFolder), pacote (grupo) ou arquivo (RenameEntry).
// Mover membro para dentro/fora de /arquivos muda a associação ao pacote.
func (v *VaultFS) Rename(oldName, newName string, ctx *fuse.Context) fuse.Status {
	oldName = cleanPath(oldName)
	newName = cleanPath(newName)
	// pendente e já fechado: importa antes de renomear (o dado precisa existir)
	if p := v.getPending(oldName); p != nil && !p.orphan {
		v.importPending(oldName, p)
		v.dropPending(oldName)
	} else if p != nil && p.orphan {
		return fuse.ENOENT
	}
	oldIsArch := strings.HasPrefix(oldName, vault.ArchiveRoot+"/")
	newIsArch := strings.HasPrefix(newName, vault.ArchiveRoot+"/")
	if oldIsArch && newIsArch {
		_, _, oldGroup, _ := vault.ParseArchivePath(oldName)
		_, _, newGroup, _ := vault.ParseArchivePath(newName)
		if v.st.HasFolder(vault.NormalizeFolderInput(oldName)) {
			v.importPendingUnder(oldName)
			if err := v.st.RenameGroup(oldGroup, newGroup); err != nil {
				return fuse.EIO
			}
			if err := v.st.RenameFolder(oldName, newName); err != nil {
				return fuse.EINVAL
			}
			v.remapPendingPath(oldName, newName, oldGroup, newGroup)
			return fuse.OK
		}
		if err := v.st.RenameEntry(oldName, newName); err != nil {
			if os.IsExist(err) {
				return fuse.Status(syscall.EEXIST)
			}
			return fuse.ENOENT
		}
		return fuse.OK
	}
	if v.st.HasFolder(vault.NormalizeFolderInput(oldName)) {
		v.importPendingUnder(oldName)
		if err := v.st.RenameFolder(oldName, newName); err != nil {
			return fuse.EINVAL
		}
		v.remapPendingPath(oldName, newName, "", "")
		return fuse.OK
	}
	if err := v.st.RenameEntry(oldName, newName); err != nil {
		if os.IsExist(err) {
			return fuse.Status(syscall.EEXIST)
		}
		return fuse.ENOENT
	}
	return fuse.OK
}

// remapPendingPath remapeia pendências quando uma pasta/pacote é renomeado.
func (v *VaultFS) remapPendingPath(oldName, newName, oldGroup, newGroup string) {
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	prefix := strings.TrimSuffix(oldName, "/") + "/"
	for pname, p := range v.pending {
		if pname != oldName && !strings.HasPrefix(pname, prefix) {
			continue
		}
		newKey := newName + strings.TrimPrefix(pname, oldName)
		v.pending[newKey] = p
		delete(v.pending, pname)
		if p.opts != nil {
			if p.opts.Folder == oldName || strings.HasPrefix(p.opts.Folder, oldName+"/") {
				p.opts.Folder = newName + strings.TrimPrefix(p.opts.Folder, oldName)
			}
			if oldGroup != "" && p.opts.Group == oldGroup {
				p.opts.Group = newGroup
			}
		}
		v.writeMeta(p)
	}
}

// OpenDir lista pastas virtuais + sintetizadas + arquivos. Escritas recém-
// fechadas no diretório são importadas aqui (listagem espera o pack).
func (v *VaultFS) OpenDir(name string, ctx *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	name = cleanPath(name)
	prefix := strings.TrimSuffix(name, "/") + "/"
	for pname, p := range v.pendingSnapshot() {
		if p.orphan || p.imported {
			continue
		}
		if strings.HasPrefix(pname, prefix) && !strings.Contains(strings.TrimPrefix(pname, prefix), "/") {
			v.importPending(pname, p)
			v.dropPending(pname)
		}
	}
	dirs := map[string]bool{}
	for _, fo := range v.st.Man.Folders {
		if fo != "/" && path.Dir(fo) == name {
			dirs[path.Base(fo)] = true
		}
	}
	files := map[string]bool{}
	for _, e := range v.st.Man.Entries {
		if !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(e.Path, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			dirs[rest[:i]] = true
		} else {
			files[rest] = true
		}
	}
	var out []fuse.DirEntry
	names := make([]string, 0, len(dirs))
	for d := range dirs {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		out = append(out, fuse.DirEntry{Name: d, Mode: fuse.S_IFDIR})
	}
	var fnames []string
	for f := range files {
		if !dirs[f] {
			fnames = append(fnames, f)
		}
	}
	sort.Strings(fnames)
	for _, f := range fnames {
		out = append(out, fuse.DirEntry{Name: f, Mode: fuse.S_IFREG})
	}
	return out, fuse.OK
}

// Access: dono tem acesso total (o cofre é do usuário que montou).
func (v *VaultFS) Access(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	return fuse.OK
}

// StatFs reflete o disco real onde o vault vive.
func (v *VaultFS) StatFs(name string) *fuse.StatfsOut {
	var st syscall.Statfs_t
	if err := syscall.Statfs(v.st.Cfg.Root, &st); err == nil {
		out := &fuse.StatfsOut{}
		out.Bsize = uint32(st.Bsize)
		out.Blocks = st.Blocks
		out.Bfree = st.Bfree
		out.Bavail = st.Bavail
		out.Files = st.Files
		out.Ffree = st.Ffree
		out.Frsize = uint32(st.Bsize)
		out.NameLen = 255
		return out
	}
	return &fuse.StatfsOut{Bsize: 4096, NameLen: 255}
}

var _ pathfs.FileSystem = (*VaultFS)(nil)

// MountServer cria o servidor FUSE do cofre (não serve; use srv.Serve()).
// Reusado pelo comando fuse e pelo tray.
func MountServer(mountpoint, root, brain string, noVerify bool) (*fuse.Server, *vault.Store, error) {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, nil, err
	}
	s, err := vault.LoadStore(root)
	if err != nil {
		return nil, nil, err
	}
	vfs := New(s, Options{Brain: brain, NoVerify: noVerify})
	nodeFs := pathfs.NewPathNodeFs(vfs, nil)
	conn := nodefs.NewFileSystemConnector(nodeFs.Root(), &nodefs.Options{})
	srv, err := fuse.NewServer(conn.RawFS(), mountpoint, MountOptions())
	if err != nil {
		return nil, nil, fmt.Errorf("montagem falhou (fuse3 instalado?): %v", err)
	}
	return srv, s, nil
}

// MountLoopbackRO monta um diretório como FUSE somente-leitura
// (usado pelo modo CD: montar um pacote portátil .cromar).
func MountLoopbackRO(srcDir, mountpoint string) (*fuse.Server, error) {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, err
	}
	loop := pathfs.NewLoopbackFileSystem(srcDir)
	ro := pathfs.NewReadonlyFileSystem(loop)
	nodeFs := pathfs.NewPathNodeFs(ro, nil)
	conn := nodefs.NewFileSystemConnector(nodeFs.Root(), &nodefs.Options{})
	srv, err := fuse.NewServer(conn.RawFS(), mountpoint, MountOptions())
	if err != nil {
		return nil, fmt.Errorf("montagem falhou (fuse3 instalado?): %v", err)
	}
	return srv, nil
}

// MountOptions para o server go-fuse.
func MountOptions() *fuse.MountOptions {
	return &fuse.MountOptions{
		AllowOther: false,
		Options:    []string{"fsname=crom-ar", "subtype=crom-ar"},
		Debug:      os.Getenv("CROM_AR_FUSE_DEBUG") != "",
	}
}

// IsMounted verifica se o caminho já aparece em /proc/mounts.
func IsMounted(mountpoint string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mountpoint {
			return true
		}
	}
	return false
}
