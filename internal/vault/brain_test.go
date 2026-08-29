package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeStoreComRoot(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	s := &Store{
		Cfg:     &Config{Root: root, VaultDir: filepath.Join(root, "vault"), BrainsDir: filepath.Join(root, "brains")},
		Man:     &Manifest{Version: manifestVersion, Folders: []string{"/", ArchiveRoot}},
		manPath: filepath.Join(root, "manifest.json"),
		timeNow: func() time.Time { return time.Unix(0, 0) },
	}
	if err := os.MkdirAll(s.Cfg.BrainsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// ════════ Codebooks: agregação de economia ════════

func TestComputeBrainStats(t *testing.T) {
	s := fakeStoreComRoot(t)
	for _, name := range []string{"vault", "codigofonte"} {
		if err := os.WriteFile(filepath.Join(s.Cfg.BrainsDir, name+".cromdb"), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.Cfg.DefaultBrain = "vault"
	abs := func(n string) string { return filepath.Join(s.Cfg.BrainsDir, n+".cromdb") }
	s.Man.Entries = []*Entry{
		// dedup: duas entradas lógicas, um .crom
		{ID: "h1", Size: 1000, CromSize: 100, CromFile: "h1.crom", Codebook: abs("vault")},
		{ID: "h1", Size: 1000, CromSize: 100, CromFile: "h1.crom", Codebook: abs("vault")},
		// incompressível: causou overhead
		{ID: "h2", Size: 500, CromSize: 620, CromFile: "h2.crom", Codebook: abs("vault"), HitRate: 0},
		// codebook específico
		{ID: "h3", Size: 800, CromSize: 300, CromFile: "h3.crom", Codebook: abs("codigofonte"), HitRate: 90},
		// bypass: sem codebook
		{ID: "h4", Size: 100, CromSize: 120, CromFile: "h4.crom"},
	}
	bs := s.ComputeBrainStats()
	if len(bs) != 3 {
		t.Fatalf("esperava 3 linhas (vault, codigofonte, bypass), obtive %d: %+v", len(bs), bs)
	}
	if !bs[0].Default || bs[0].Name != "vault" {
		t.Errorf("padrão deveria vir primeiro: %+v", bs[0])
	}
	// vault: 3 entradas (2 dedup do h1 + h2 incompressível), 2 únicos
	if bs[0].Members != 3 || bs[0].Unique != 2 || bs[0].Original != 2500 || bs[0].Stored != 720 {
		t.Errorf("vault agregado errado: %+v", bs[0])
	}
	if bs[0].Saved != 1780 {
		t.Errorf("vault Saved = %d, want 1780", bs[0].Saved)
	}
	if bs[0].DBSize != 2 {
		t.Errorf("vault DBSize = %d, want 2 (tamanho do .cromdb)", bs[0].DBSize)
	}
	// codigofonte: hit médio, ratio
	if bs[1].Name != "codigofonte" || bs[1].Members != 1 || bs[1].Saved != 500 || bs[1].AvgHit != 90 {
		t.Errorf("codigofonte agregado errado: %+v", bs[1])
	}
	// bypass: nome vazio, saved negativo
	if bs[2].Name != "" || bs[2].Saved != -20 {
		t.Errorf("bypass agregado errado: %+v", bs[2])
	}
	// coerência global: soma das linhas = totais do ComputeStats
	var sumOrig, sumStored int64
	for _, b := range bs {
		sumOrig += b.Original
		sumStored += b.Stored
	}
	st := s.ComputeStats()
	if sumOrig != st.Original || sumStored != st.Stored {
		t.Errorf("linhas por codebook não somam ao global: orig=%d stored=%d vs %v", sumOrig, sumStored, st)
	}
}

func TestComputeBrainStatsVazio(t *testing.T) {
	s := fakeStoreComRoot(t)
	if bs := s.ComputeBrainStats(); len(bs) != 0 {
		t.Fatalf("cofre vazio deveria devolver lista vazia, obtive %+v", bs)
	}
}

// ════════ Modo persistido ════════

func TestNewEntryDerivaModo(t *testing.T) {
	src := filepath.Join(t.TempDir(), "app")
	if err := os.WriteFile(src, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// sem Mode explícito: deriva do arquivo fonte
	e := fakeStoreComRoot(t).newEntry("h", src, "g", "app", false, 3, "h.crom", 100, "", AddOptions{})
	if e.Mode != 0o755 {
		t.Errorf("Mode derivado = %o, want 755", e.Mode)
	}
	// com Mode explícito (vindo do FUSE), prevalece sobre o fonte
	e = fakeStoreComRoot(t).newEntry("h", src, "g", "rel", false, 10, "h.crom", 20, "", AddOptions{Mode: 0o600})
	if e.Mode != 0o600 {
		t.Errorf("Mode explícito = %o, want 600", e.Mode)
	}
}

func TestSetEntryMode(t *testing.T) {
	s := fakeStoreComRoot(t)
	e := &Entry{UID: "u1", ID: "h", Name: "app", RelPath: "app", Path: "/app", CromFile: "h.crom"}
	s.Man.Entries = append(s.Man.Entries, e)
	if err := s.SetEntryMode("/app", 0o755|0o100000); err != nil { // S_IFREG vem do kernel
		t.Fatalf("SetEntryMode: %v", err)
	}
	if e.Mode&0o777 != 0o755 {
		t.Errorf("Mode persistido = %o, want bits 755", e.Mode)
	}
	if err := s.SetEntryMode("/nao-existe", 0o755); err == nil {
		t.Error("entrada inexistente deveria falhar")
	}
}

// ════════ RemoveBrain com referências ════════

func TestRemoveBrainGuardReferencias(t *testing.T) {
	s := fakeStoreComRoot(t)
	if err := os.WriteFile(filepath.Join(s.Cfg.BrainsDir, "b1.cromdb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Cfg.DefaultBrain = "padrao"
	if err := os.WriteFile(filepath.Join(s.Cfg.BrainsDir, "padrao.cromdb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(s.Cfg.BrainsDir, "b1.cromdb")
	s.Man.Entries = []*Entry{
		{ID: "h1", Name: "a", CromFile: "h1.crom", Codebook: abs},
	}
	err := s.RemoveBrain("b1")
	if err == nil {
		t.Fatal("remove de codebook com referências deveria ser recusado")
	}
	if !strings.Contains(err.Error(), "1 entrada") {
		t.Errorf("mensagem deveria citar as referências: %v", err)
	}
	// sem referências: remove
	s.Man.Entries = nil
	if err := s.RemoveBrain("b1"); err != nil {
		t.Fatalf("sem referências deveria remover: %v", err)
	}
	// padrão continua bloqueado
	s.Man.Entries = nil
	if err := s.RemoveBrain("padrao"); err == nil {
		t.Fatal("remove do padrão deveria ser recusado")
	}
}
