package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

// newTestServer sobe o Handler real com token configurado.
func newTestServer(t *testing.T) (*httptest.Server, string, *vault.Store) {
	t.Helper()
	// stub do motor: só precisa existir (LoadStore faz stat)
	bin := filepath.Join(t.TempDir(), "crompressor")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CROMPRESSOR_BIN", bin)
	st, err := vault.LoadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sv := New(st, "127.0.0.1:0")
	sv.Token = "tok-teste-0123456789"
	ts := httptest.NewServer(sv.Handler())
	t.Cleanup(ts.Close)
	return ts, sv.Token, st
}

func code(t *testing.T, ts *httptest.Server, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTokenObrigatorio(t *testing.T) {
	ts, tok, _ := newTestServer(t)
	cases := []struct {
		nome string
		req  *http.Request
		want int
	}{
		{"api sem token", mustGet(ts.URL + "/api/stats"), 403},
		{"api token errado", mustGet(ts.URL + "/api/stats?t=ruim"), 403},
		{"página sem token", mustGet(ts.URL + "/"), 403},
		{"api token via query", mustGet(ts.URL + "/api/stats?t=" + tok), 200},
		{"api token via header", mustGetH(ts.URL+"/api/stats", tok), 200},
		{"página com token", mustGet(ts.URL + "/?t=" + tok), 200},
	}
	for _, c := range cases {
		if got := code(t, ts, c.req); got != c.want {
			t.Errorf("%s: got %d, want %d", c.nome, got, c.want)
		}
	}
}

func TestHostEOriginLocais(t *testing.T) {
	ts, tok, _ := newTestServer(t)
	r := mustGet(ts.URL + "/api/stats?t=" + tok)
	r.Host = "evil.com"
	if got := code(t, ts, r); got != 403 {
		t.Errorf("Host externo: got %d, want 403 (DNS rebinding)", got)
	}
	r, err := http.NewRequest("POST", ts.URL+"/api/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "127.0.0.1"
	r.Header.Set("Origin", "http://evil.com")
	r.Header.Set("X-Crom-Token", tok)
	if got := code(t, ts, r); got != 403 {
		t.Errorf("Origin maliciosa: got %d, want 403", got)
	}
}

func Test403AmigavelNaRaiz(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("got %d, want 403", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "atalho CROM-AR") {
		t.Error("página de bloqueio deveria explicar como abrir")
	}
}

func TestBrainsEndpoint(t *testing.T) {
	ts, tok, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/brains?t=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var j struct {
		Brains []json.RawMessage `json:"brains"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		t.Fatalf("JSON inválido: %v", err)
	}
	if j.Brains == nil {
		t.Error("cofre vazio deveria devolver lista (não null)")
	}
}

func TestOpenPathRecusaInvalidos(t *testing.T) {
	ts, tok, _ := newTestServer(t)
	// caminhos inválidos falham ANTES de chamar xdg-open (sem efeitos colaterais)
	for _, p := range []string{"/tmp/nao-existe-xyz", "/etc/passwd"} {
		r := mustGet(ts.URL + "/api/open?path=" + p + "&t=" + tok)
		if got := code(t, ts, r); got != 400 {
			t.Errorf("path %q: got %d, want 400 (só diretório existente passa)", p, got)
		}
	}
}

// Regressão da Store compartilhada (28/08): o FUSE e o painel dividem a
// mesma Store — importações concorrentes não podem rasgar o encode JSON.
// Execute com -race (make test-race) para validar.
func TestTreeSobConcorrencia(t *testing.T) {
	ts, tok, st := newTestServer(t)
	done := make(chan struct{})
	go func() { // simula mutações do FUSE (público: CreateFolder persiste)
		defer close(done)
		for i := 0; i < 60; i++ {
			if err := st.CreateFolder("/pasta" + fmt.Sprint(i)); err != nil {
				t.Log(err)
			}
		}
	}()
	for i := 0; i < 150; i++ { // painel consultando em paralelo
		resp, err := http.Get(ts.URL + "/api/tree?t=" + tok)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	<-done
}

func mustGet(url string) *http.Request {
	r, err := http.NewRequest("GET", url, nil)
	if err != nil {
		panic(err)
	}
	return r
}

func mustGetH(url, tok string) *http.Request {
	r := mustGet(url)
	r.Header.Set("X-Crom-Token", tok)
	return r
}
