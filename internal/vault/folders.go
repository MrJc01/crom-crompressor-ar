package vault

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

// ByUID localiza uma entrada pelo UID estável.
func (s *Store) ByUID(uid string) *Entry {
	for _, e := range s.Man.Entries {
		if e.UID == uid {
			return e
		}
	}
	return nil
}

// EntryByPath localiza a entrada pelo caminho canônico de exibição.
func (s *Store) EntryByPath(p string) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entryByPathLocked(p)
}

func (s *Store) entryByPathLocked(p string) *Entry {
	p = "/" + strings.Trim(path.Clean("/"+p), "/")
	for _, e := range s.Man.Entries {
		if e.Path == p {
			return e
		}
	}
	return nil
}

// RenameEntry move/renomeia a entrada para um novo caminho canônico.
// Movendo para dentro/fora de /arquivos ajusta o grupo (pacote).
func (s *Store) RenameEntry(oldPath, newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldPath = "/" + strings.Trim(path.Clean("/"+oldPath), "/")
	newPath = "/" + strings.Trim(path.Clean("/"+newPath), "/")
	if oldPath == newPath {
		return nil
	}
	if s.entryByPathLocked(newPath) != nil {
		return os.ErrExist
	}
	e := s.entryByPathLocked(oldPath)
	if e == nil {
		return os.ErrNotExist
	}
	e.Path = newPath
	e.Name = path.Base(newPath)
	e.Folder = NormalizeFolderInput(path.Dir(newPath))
	e.RelPath = path.Base(newPath)
	if isArch, _, group, rel := ParseArchivePath(newPath); isArch && rel != group {
		// virou membro de pacote
		e.Group = group
		e.RelPath = rel
		e.IsDir = true
	} else if !isArch || rel == group {
		// arquivo independente (fora de /arquivos ou solto na raiz dela)
		e.Group = path.Base(newPath)
		e.RelPath = path.Base(newPath)
		e.IsDir = false
	}
	return s.Save()
}

// RemoveByPath remove a entrada que ocupa o caminho informado.
func (s *Store) RemoveByPath(p string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryByPathLocked(p)
	if e == nil {
		return false, nil
	}
	_, err := s.removeLocked([]*Entry{e})
	return true, err
}

// EntriesIn retorna as entradas de uma pasta (inclui subpastas se recursive).
func (s *Store) EntriesIn(folder string, recursive bool) []*Entry {
	var out []*Entry
	prefix := strings.TrimSuffix(folder, "/") + "/"
	for _, e := range s.Man.Entries {
		if e.Folder == folder || (recursive && strings.HasPrefix(e.Folder, prefix)) {
			out = append(out, e)
		}
	}
	return out
}

// SubFolders retorna as subpastas diretas de uma pasta.
func (s *Store) SubFolders(folder string) []string {
	prefix := strings.TrimSuffix(folder, "/") + "/"
	set := map[string]bool{}
	for _, f := range s.Man.Folders {
		if f != folder && strings.HasPrefix(f, prefix) {
			rest := strings.TrimPrefix(f, prefix)
			if i := strings.Index(rest, "/"); i >= 0 {
				rest = rest[:i]
			}
			if rest != "" {
				set[prefix+rest] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// CreateFolder cria uma pasta virtual (caminho completo, ex: "/projetos/2026").
func (s *Store) CreateFolder(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = normalizeFolder(p)
	if p == "/" {
		return fmt.Errorf("raiz já existe")
	}
	for _, f := range s.Man.Folders {
		if f == p {
			return fmt.Errorf("pasta já existe: %s", p)
		}
	}
	// garante ancestrais
	parts := strings.Split(strings.Trim(p, "/"), "/")
	cur := ""
	for _, part := range parts {
		cur += "/" + part
		if !s.hasFolder(cur) {
			s.Man.Folders = append(s.Man.Folders, cur)
		}
	}
	return s.Save()
}

// RenameFolder renomeia uma pasta (e remapeia descendentes e entradas).
func (s *Store) RenameFolder(old, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old = normalizeFolder(old)
	if old == "/" {
		return fmt.Errorf("não é possível renomear a raiz")
	}
	parent := path.Dir(old)
	dst := normalizeFolder(parent + "/" + path.Base(newName))
	if dst == old {
		return nil
	}
	if s.hasFolder(dst) {
		return fmt.Errorf("já existe: %s", dst)
	}
	if !s.hasFolder(old) {
		return fmt.Errorf("pasta não encontrada: %s", old)
	}
	newList := []string{"/"}
	for _, f := range s.Man.Folders {
		switch {
		case f == old:
			newList = append(newList, dst)
		case strings.HasPrefix(f, old+"/"):
			newList = append(newList, dst+strings.TrimPrefix(f, old))
		default:
			newList = append(newList, f)
		}
	}
	s.Man.Folders = dedupeFolders(newList)
	for _, e := range s.Man.Entries {
		if e.Folder == old {
			e.Folder = dst
		} else if strings.HasPrefix(e.Folder, old+"/") {
			e.Folder = dst + strings.TrimPrefix(e.Folder, old)
		}
		if e.Path == old {
			e.Path = dst
		} else if strings.HasPrefix(e.Path, old+"/") {
			e.Path = dst + strings.TrimPrefix(e.Path, old)
		}
	}
	return s.Save()
}

// DeleteFolder remove a pasta (deve estar vazia de subpastas) e move as
// entradas para a pasta pai.
func (s *Store) DeleteFolder(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = normalizeFolder(p)
	if p == "/" {
		return fmt.Errorf("não é possível remover a raiz")
	}
	if !s.hasFolder(p) {
		return fmt.Errorf("pasta não encontrada: %s", p)
	}
	for _, f := range s.Man.Folders {
		if f != p && strings.HasPrefix(f, p+"/") {
			return fmt.Errorf("pasta contém subpastas — remova-as primeiro")
		}
	}
	parent := path.Dir(p)
	for _, e := range s.Man.Entries {
		if e.Folder == p {
			e.Folder = parent
			e.Path = path.Join(parent, path.Base(e.Path))
		}
	}
	var keep []string
	for _, f := range s.Man.Folders {
		if f != p {
			keep = append(keep, f)
		}
	}
	s.Man.Folders = keep
	return s.Save()
}

// MoveEntry move uma entrada para outra pasta virtual.
func (s *Store) MoveEntry(uid, folder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.ByUID(uid)
	if e == nil {
		return fmt.Errorf("entrada não encontrada: %s", uid)
	}
	folder = normalizeFolder(folder)
	if !s.hasFolder(folder) {
		return fmt.Errorf("pasta não encontrada: %s", folder)
	}
	suffix := e.Path
	if e.Folder != "/" {
		suffix = strings.TrimPrefix(e.Path, e.Folder)
	}
	e.Folder = folder
	if folder == "/" {
		e.Path = suffix
	} else {
		e.Path = folder + suffix
	}
	return s.Save()
}

// ParseArchivePath interpreta um caminho na vista de pacotes (/arquivos).
// Retorna (éPacote, pastaVirtual, grupo, relPath). rel==group significa
// arquivo solto direto em /arquivos (pacote de arquivo único).
func ParseArchivePath(p string) (isArchive bool, folder, group, rel string) {
	p = "/" + strings.Trim(path.Clean("/"+p), "/")
	if !strings.HasPrefix(p, ArchiveRoot+"/") {
		return false, "", "", ""
	}
	rest := strings.TrimPrefix(p, ArchiveRoot+"/")
	if rest == "" {
		return true, ArchiveRoot, "", ""
	}
	i := strings.Index(rest, "/")
	if i < 0 {
		return true, ArchiveRoot, rest, rest
	}
	group = rest[:i]
	return true, ArchiveRoot + "/" + group, group, rest[i+1:]
}

// GroupHasEntries informa se algum membro pertence ao grupo (pacote).
func (s *Store) GroupHasEntries(group string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.Man.Entries {
		if e.Group == group {
			return true
		}
	}
	return false
}

// RenameGroup renomeia um pacote inteiro (grupo) e remapeia os caminhos.
func (s *Store) RenameGroup(old, new string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldPrefix := ArchiveRoot + "/" + old
	newPrefix := ArchiveRoot + "/" + new
	for _, e := range s.Man.Entries {
		if e.Group != old {
			continue
		}
		e.Group = new
		if e.Path == oldPrefix || strings.HasPrefix(e.Path, oldPrefix+"/") {
			e.Path = newPrefix + strings.TrimPrefix(e.Path, oldPrefix)
		}
		if e.Folder == oldPrefix || strings.HasPrefix(e.Folder, oldPrefix+"/") {
			e.Folder = newPrefix + strings.TrimPrefix(e.Folder, oldPrefix)
		}
	}
	return s.Save()
}

// HasFolder informa se a pasta virtual existe.
func (s *Store) HasFolder(p string) bool {
	if p == "/" {
		return true
	}
	for _, f := range s.Man.Folders {
		if f == p {
			return true
		}
	}
	return false
}

func (s *Store) hasFolder(p string) bool {
	return s.HasFolder(p)
}

func normalizeFolder(p string) string {
	p = "/" + strings.Trim(path.Clean("/"+strings.TrimSpace(p)), "/")
	if p == "//" {
		p = "/"
	}
	return path.Clean(p)
}

// NormalizeFolderInput normaliza um caminho de pasta virtual (uso da API).
func NormalizeFolderInput(p string) string { return normalizeFolder(p) }

func dedupeFolders(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range in {
		if f == "" {
			f = "/"
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if !seen["/"] {
		out = append([]string{"/"}, out...)
	}
	sort.Strings(out)
	return out
}
