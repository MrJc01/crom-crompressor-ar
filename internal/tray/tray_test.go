package tray

import "testing"

// Regressão: o default de WebAddr era aplicado DEPOIS de copiar opt para o
// App — app.opt.WebAddr ficava "" e openPanel montava "http:///?t=...".
func TestNormalizeOptionsWebAddr(t *testing.T) {
	got := normalizeOptions(Options{})
	if got.WebAddr != "127.0.0.1:8619" {
		t.Fatalf("WebAddr default = %q, want 127.0.0.1:8619", got.WebAddr)
	}
	got = normalizeOptions(Options{WebAddr: "127.0.0.1:9000"})
	if got.WebAddr != "127.0.0.1:9000" {
		t.Fatalf("WebAddr explícito não deveria mudar: %q", got.WebAddr)
	}
	got = normalizeOptions(Options{Mountpoint: "/mnt/x", NoWeb: true})
	if got.Mountpoint != "/mnt/x" || !got.NoWeb {
		t.Fatalf("demais opções deveriam ser preservadas: %+v", got)
	}
}

// openPanel monta a URL a partir de app.opt.WebAddr — com o default aplicado,
// a URL precisa ter host e token (bug original: "http:///?t=...").
func TestOpenPanelURLTemHost(t *testing.T) {
	app := &App{opt: normalizeOptions(Options{})}
	if app.opt.WebAddr == "" {
		t.Fatal("app.opt.WebAddr vazio — reprodução do bug do painel")
	}
}
