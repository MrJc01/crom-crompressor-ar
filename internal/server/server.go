// Package server expõe o cofre crom-ar como uma GUI local (estilo WinRAR)
// acessível pelo navegador em 127.0.0.1.
package server

import (
	"context"
	"crypto/rand"
	_ "embed"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

//go:embed static/index.html
var indexHTML []byte

// Server atende a GUI local.
type Server struct {
	Store *vault.Store
	Addr  string
	Token string // exigido em toda API (header X-Crom-Token ou ?t=)

	muSrv   sync.Mutex
	httpSrv *http.Server
}

// New cria o servidor.
func New(store *vault.Store, addr string) *Server {
	if addr == "" {
		addr = "127.0.0.1:8619"
	}
	return &Server{Store: store, Addr: addr}
}

// DefaultTokenPath é onde o token de acesso à GUI fica guardado (0600).
func DefaultTokenPath() string {
	if p := os.Getenv("CROM_AR_TOKEN_FILE"); p != "" {
		return vault.ExpandHome(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "crom-ar.token"
	}
	return filepath.Join(home, ".local", "share", "crom-ar", "token")
}

// EnsureToken garante que exista um token (cria com 0600 se necessário).
func EnsureToken() (string, error) {
	p := DefaultTokenPath()
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func (sv *Server) checkToken(r *http.Request) bool {
	if sv.Token == "" {
		return true // sem token configurado (modo legado/teste)
	}
	q := r.URL.Query().Get("t")
	h := r.Header.Get("X-Crom-Token")
	got := q
	if h != "" {
		got = h
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(sv.Token)) == 1
}

// hostLocal aceita apenas loopback no Host (defesa contra DNS rebinding).
func (sv *Server) hostLocal(r *http.Request) bool {
	h := r.Host
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "[::1]"
}

// secure protege todas as rotas: Host local + Origin (se enviada) + token.
func (sv *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sv.hostLocal(r) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if orig := r.Header.Get("Origin"); orig != "" {
			u, err := url.Parse(orig)
			if err != nil {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		if !sv.checkToken(r) {
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<!DOCTYPE html><html lang="pt-BR"><head><meta charset="utf-8">
<title>CROM-AR — acesso</title><style>body{font:15px system-ui;background:#0f1116;color:#e8eaf0;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#171a21;border:1px solid #2a3040;border-radius:14px;padding:32px;max-width:440px}
h1{font-size:18px}code{background:#0f1116;padding:2px 6px;border-radius:6px}
a{color:#5eead4}</style></head><body><div class="card">
<h1>🧬 CROM-AR precisa do token de acesso</h1>
<p>Este painel é local e protegido. Abra pelo <b>atalho CROM-AR</b> na área de
trabalho, pelo ícone na bandeija (clique → <i>Abrir painel</i>) ou rode:</p>
<p><code>crom-ar gui</code></p>
<p style="color:#8b93a7">O link correto contém <code>?t=&lt;token&gt;</code> —
o token fica em <code>~/.local/share/crom-ar/token</code> (chmod 0600).</p>
</div></body></html>`)
				return
			}
			http.Error(w, "token inválido — abra pelo atalho crom-ar", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PortBusy informa se a porta já está em uso (outra instância rodando).
func PortBusy(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// Handler monta o mux HTTP protegido pelo middleware secure.
func (sv *Server) Handler() http.Handler {
	return sv.secure(sv.routes())
}

func (sv *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", sv.home)
	mux.HandleFunc("GET /api/tree", sv.tree)
	mux.HandleFunc("GET /api/stats", sv.stats)
	mux.HandleFunc("POST /api/folder/create", sv.folderCreate)
	mux.HandleFunc("POST /api/folder/rename", sv.folderRename)
	mux.HandleFunc("POST /api/folder/delete", sv.folderDelete)
	mux.HandleFunc("POST /api/move", sv.move)
	mux.HandleFunc("POST /api/entries/delete", sv.entriesDelete)
	mux.HandleFunc("POST /api/extract", sv.extract)
	mux.HandleFunc("POST /api/import", sv.importFiles)
	mux.HandleFunc("POST /api/train", sv.train)
	mux.HandleFunc("GET /api/info", sv.info)
	mux.HandleFunc("GET /api/open", sv.openPath)
	mux.HandleFunc("GET /api/packages", sv.packages)
	mux.HandleFunc("GET /api/search", sv.search)
	return mux
}

// ListenAndServe bloqueia servindo a GUI.
func (sv *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", sv.Addr)
	if err != nil {
		return err
	}
	sv.muSrv.Lock()
	sv.httpSrv = &http.Server{Handler: sv.Handler()}
	sv.muSrv.Unlock()
	return sv.httpSrv.Serve(ln)
}

// Stop encerra o servidor web com elegância (usado pelo toggle do tray).
func (sv *Server) Stop() {
	sv.muSrv.Lock()
	srv := sv.httpSrv
	sv.httpSrv = nil
	sv.muSrv.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func (sv *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

type treeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Children []*treeNode `json:"children"`
}

func buildTree(folders []string) []*treeNode {
	root := &treeNode{Name: "Cofre", Path: "/", Children: nil}
	byPath := map[string]*treeNode{"/": root}
	sorted := append([]string(nil), folders...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	for _, f := range sorted {
		if f == "/" {
			continue
		}
		name := f[strings.LastIndex(f[1:], "/")+1:]
		node := &treeNode{Name: name, Path: f}
		byPath[f] = node
		parent := byPath[parentNode(f)]
		parent.Children = append(parent.Children, node)
	}
	return root.Children
}

func parentNode(p string) string {
	i := strings.LastIndex(p[:len(p)-1], "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func (sv *Server) tree(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"folders": buildTree(sv.Store.Man.Folders),
		"flat":    sv.Store.Man.Folders,
		"entries": sv.Store.Man.Entries,
	})
}

func (sv *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, sv.Store.ComputeStats())
}

func (sv *Server) folderCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if err := sv.Store.CreateFolder(req.Path); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) folderRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if err := sv.Store.RenameFolder(req.Path, req.Name); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) folderDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if err := sv.Store.DeleteFolder(req.Path); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) move(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs   []string `json:"uids"`
		Folder string   `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	for _, uid := range req.UIDs {
		if err := sv.Store.MoveEntry(uid, req.Folder); err != nil {
			fail(w, 400, err)
			return
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) entriesDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	var entries []*vault.Entry
	for _, uid := range req.UIDs {
		if e := sv.Store.ByUID(uid); e != nil {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		fail(w, 404, fmt.Errorf("nada para remover"))
		return
	}
	n, err := sv.Store.Remove(entries)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "removed": n})
}

func (sv *Server) extract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs      []string `json:"uids,omitempty"`
		Folder    string   `json:"folder,omitempty"`
		Recursive bool     `json:"recursive,omitempty"`
		Dest      string   `json:"dest"`
		Force     bool     `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if !filepath.IsAbs(vault.ExpandHome(req.Dest)) {
		fail(w, 400, fmt.Errorf("destino deve ser um caminho absoluto"))
		return
	}
	var entries []*vault.Entry
	if len(req.UIDs) > 0 {
		for _, uid := range req.UIDs {
			if e := sv.Store.ByUID(uid); e != nil {
				entries = append(entries, e)
			}
		}
	} else if req.Folder != "" {
		entries = sv.Store.EntriesIn(req.Folder, req.Recursive)
	}
	if len(entries) == 0 {
		fail(w, 404, fmt.Errorf("nada para extrair"))
		return
	}
	results, err := sv.Store.Extract(entries, vault.ExtractOptions{OutDir: req.Dest, Force: req.Force})
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"results": results})
}

// importFiles recebe upload multipart (f0/r0, f1/r1, ...) e empacota no cofre.
func (sv *Server) importFiles(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		fail(w, 400, err)
		return
	}
	target := vault.NormalizeFolderInput(r.FormValue("folder"))
	tmp, err := os.MkdirTemp("", "crom-ar-upload-*")
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer os.RemoveAll(tmp)

	count := 0
	saved := 0
	for {
		fh := r.MultipartForm.File["f"+strconv.Itoa(count)]
		if len(fh) == 0 {
			break
		}
		rel := r.FormValue("r"+strconv.Itoa(count))
		if rel == "" {
			rel = fh[0].Filename
		}
		rel = filepath.Clean("/" + rel)
		dst := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fail(w, 500, err)
			return
		}
		src, err := fh[0].Open()
		if err != nil {
			fail(w, 500, err)
			return
		}
		out, err := os.Create(dst)
		if err != nil {
			src.Close()
			fail(w, 500, err)
			return
		}
		if _, err := io.Copy(out, src); err != nil {
			src.Close()
			out.Close()
			fail(w, 500, err)
			return
		}
		src.Close()
		out.Close()
		saved++
		count++
	}
	if saved == 0 {
		fail(w, 400, fmt.Errorf("nenhum arquivo recebido"))
		return
	}

	// empacota: uploads de pasta (com subdiretórios) usam a pasta tmp inteira
	hasSubdirs := false
	_ = filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != tmp {
			hasSubdirs = true
		}
		return nil
	})

	var results []vault.ImportResult
	opt := vault.AddOptions{CDC: true}
	if hasSubdirs {
		res, err := sv.Store.Add(tmp, opt)
		if err != nil {
			fail(w, 500, err)
			return
		}
		results = res
	} else {
		files, _ := filepath.Glob(filepath.Join(tmp, "*"))
		for _, f := range files {
			res, err := sv.Store.Add(f, opt)
			if err != nil {
				continue
			}
			results = append(results, res...)
		}
	}

	// organiza nas pastas virtuais alvo
	if !sv.Store.HasFolder(target) {
		_ = sv.Store.CreateFolder(target)
	}
	for _, res := range results {
		e := res.Entry
		if e.IsDir {
			// group = pasta raiz do upload (nomeia a pasta recompilada no extract)
			rel := filepath.ToSlash(e.RelPath)
			if i := strings.Index(rel, "/"); i > 0 {
				e.Group = rel[:i]
			}
			dir := filepath.Dir(rel)
			if dir != "." && dir != "/" {
				_ = sv.Store.CreateFolder(target + "/" + dir)
				e.Folder = vault.NormalizeFolderInput(target + "/" + dir)
			} else {
				e.Folder = target
			}
		} else {
			e.Folder = target
		}
		// vista de pacotes: alvo em /arquivos/<grupo> vira membro do grupo
		if isArch, folder, grp, rel := vault.ParseArchivePath(target); isArch && grp != "" {
			_ = rel
			memberPath := vault.NormalizeFolderInput(target + "/" + e.RelPath)
			if _, _, _, memberRel := vault.ParseArchivePath(memberPath); memberRel != grp {
				// dentro de um pacote: associar ao grupo
				e.Group = grp
				e.Folder = folder
				if rel != grp {
					e.RelPath = rel + "/" + e.RelPath
				}
			}
		}
	}
	if err := sv.Store.Save(); err != nil {
		fail(w, 500, err)
		return
	}
	var summary []map[string]any
	for _, res := range results {
		summary = append(summary, map[string]any{
			"name": res.Entry.RelPath, "size": res.Entry.Size,
			"crom": res.Entry.CromSize, "dedup": res.Dedup, "msg": res.Msg,
		})
	}
	writeJSON(w, map[string]any{"ok": true, "imported": len(summary), "results": summary})
}

func (sv *Server) train(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir  string `json:"dir,omitempty"`
		Size int    `json:"size,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if req.Size <= 0 {
		req.Size = 8192
	}
	if req.Dir == "" {
		if len(sv.Store.Man.Entries) == 0 {
			fail(w, 400, fmt.Errorf("cofre vazio: informe um diretório"))
			return
		}
		dirs := map[string]bool{}
		for _, e := range sv.Store.Man.Entries {
			d := filepath.Dir(e.SourcePath)
			if !dirs[d] && len(dirs) < 8 {
				dirs[d] = true
			}
		}
		for d := range dirs {
			if _, err := sv.Store.TrainBrainUpdate(d, req.Size); err != nil {
				fail(w, 500, err)
				return
			}
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	cb, err := sv.Store.TrainBrainUpdate(vault.ExpandHome(req.Dir), req.Size)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "codebook": cb})
}

func (sv *Server) info(w http.ResponseWriter, r *http.Request) {
	e := sv.Store.ByUID(r.URL.Query().Get("uid"))
	if e == nil {
		fail(w, 404, fmt.Errorf("entrada não encontrada"))
		return
	}
	args := []string{"info", "-i", sv.Store.CromPath(e)}
	if e.Codebook != "" {
		args = append(args, "-c", e.Codebook)
	}
	out, err := sv.Store.Run(args...)
	if err != nil {
		out += "\n" + err.Error()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(out))
}

// packages retorna o relatório por pacote/grupo (membros, ratio, hit rate).
func (sv *Server) packages(w http.ResponseWriter, r *http.Request) {
	type pkgInfo struct {
		Group    string   `json:"group"`
		Members  int      `json:"members"`
		Original int64    `json:"original"`
		Crom     int64    `json:"crom"`
		Ratio    float64  `json:"ratio"`
		Brain    string   `json:"brain"`
		AvgHit   float64  `json:"avg_hit"`
		Files    []string `json:"files"`
	}
	order := []string{}
	byGroup := map[string]*pkgInfo{}
	for _, e := range sv.Store.Man.Entries {
		g := e.Group
		if g == "" {
			g = e.Name
		}
		p := byGroup[g]
		if p == nil {
			p = &pkgInfo{Group: g, Brain: e.Codebook}
			byGroup[g] = p
			order = append(order, g)
		}
		p.Members++
		p.Original += e.Size
		p.Crom += e.CromSize
		p.AvgHit += e.HitRate
		if n := path.Base(e.Path); len(p.Files) < 6 {
			p.Files = append(p.Files, n)
		}
	}
	out := make([]*pkgInfo, 0, len(order))
	for _, g := range order {
		p := byGroup[g]
		if p.Original > 0 {
			p.Ratio = float64(p.Crom) / float64(p.Original) * 100
		}
		if p.Members > 0 {
			p.AvgHit /= float64(p.Members)
		}
		out = append(out, p)
	}
	writeJSON(w, out)
}

// search roda o grep O(1) do crompressor nos .crom dos membros (limite 40).
func (sv *Server) search(w http.ResponseWriter, r *http.Request) {
	term := r.URL.Query().Get("q")
	if term == "" {
		fail(w, 400, fmt.Errorf("query vazia"))
		return
	}
	type hit struct {
		Name string `json:"name"`
		Line string `json:"line"`
	}
	hits := []hit{}
	scanned := 0
	cb := sv.Store.DefaultBrainPath()
	for _, e := range sv.Store.Man.Entries {
		if scanned >= 40 {
			break
		}
		out, err := sv.Store.Run("grep", term, "-i", sv.Store.CromPath(e))
		if err != nil {
			continue
		}
		scanned++
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && len(hits) < 50 {
				hits = append(hits, hit{Name: e.Name, Line: line})
			}
		}
	}
	_ = cb
	writeJSON(w, map[string]any{"hits": hits, "scanned": scanned})
}

func (sv *Server) openPath(w http.ResponseWriter, r *http.Request) {
	p := vault.ExpandHome(r.URL.Query().Get("path"))
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		fail(w, 400, fmt.Errorf("diretório inválido: %s", p))
		return
	}
	if err := exec.Command("xdg-open", p).Start(); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
