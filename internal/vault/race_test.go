package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Regressão da Store compartilhada (28/08): FUSE grava (append) enquanto
// leitores (painel web) tiram snapshots e agregam — execute com -race.
func TestMutacaoConcorrenteComLeitores(t *testing.T) {
	s := fakeStoreComRoot(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // produtor (papel do FUSE importando)
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.appendEntry(&Entry{UID: fmt.Sprint(i), ID: "h", Name: "a", RelPath: "a", Path: "/a" + fmt.Sprint(i), CromFile: "x.crom", Size: 1})
			s.appendFolder("/p" + fmt.Sprint(i%7))
		}
	}()
	go func() { // leitores (papel do painel web)
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_ = s.EntriesSnapshot()
			_ = s.FoldersSnapshot()
			_ = s.ComputeStats()
			_ = s.ComputeBrainStats()
		}
	}()
	wg.Wait()
	if got := len(s.EntriesSnapshot()); got != 500 {
		t.Fatalf("entradas = %d, want 500", got)
	}
}

// ════════ T14a: consistência residente × CLI ════════
// Duas Stores no MESMO root simulando o residente (tray) e um CLI.
// O disco é a autoridade; merges por UID não podem perder mutações.

func loadPair(t *testing.T) (*Store, *Store) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "crompressor")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CROMPRESSOR_BIN", bin)
	root := t.TempDir()
	s1, err := LoadStore(root) // papel do residente
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadStore(root) // papel do CLI (processo separado, memória própria)
	if err != nil {
		t.Fatal(err)
	}
	return s1, s2
}

func entryFor(name string) *Entry {
	return &Entry{UID: "uid-" + name, ID: "h-" + name, Name: name, RelPath: name,
		Path: "/" + name, CromFile: "h-" + name + ".crom", Size: 10, CromSize: 10}
}

func manifestByUID(root string) map[string]bool {
	t2 := &Manifest{}
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil
	}
	_ = json.Unmarshal(b, t2)
	out := map[string]bool{}
	for _, e := range t2.Entries {
		out[e.UID] = true
	}
	return out
}

// 1) CLI adiciona e salva; o residente salva DEPOIS — a entrada do CLI sobrevive.
func TestResidenteNaoSobrescreveCLI(t *testing.T) {
	s1, s2 := loadPair(t)
	s2.appendEntry(entryFor("do-cli"))
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}
	s1.appendEntry(entryFor("do-residente"))
	if err := s1.Save(); err != nil {
		t.Fatal(err)
	}
	disk := manifestByUID(s1.Cfg.Root)
	if !disk["uid-do-cli"] || !disk["uid-do-residente"] {
		t.Fatalf("merge perdeu entrada: disco tem %v", disk)
	}
	if n := len(s1.EntriesSnapshot()); n != 2 {
		t.Fatalf("memória do residente = %d, want 2", n)
	}
}

// 2) Residente remove; CLI com memória velha (ainda tem a entrada) salva —
// o merge do CLI adota o disco e a remoção se mantém (sem ressurreição).
func TestRemocaoResidenteSobreviveAoCLI(t *testing.T) {
	s1, s2 := loadPair(t)
	s1.appendEntry(entryFor("vai-morrer"))
	if err := s1.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Save(); err != nil { // CLI ainda não conhece a entrada
		t.Fatal(err)
	}
	// CLI carrega a entrada (agora existe no disco)...
	s2, _ = LoadStore(s1.Cfg.Root)
	// ...o residente remove...
	if _, err := s1.Remove(s1.EntriesSnapshot()); err != nil {
		t.Fatal(err)
	}
	// ...e o CLI velho salva com a entrada ainda na memória
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}
	disk := manifestByUID(s1.Cfg.Root)
	if disk["uid-vai-morrer"] {
		t.Fatal("entrada removida pelo residente ressuscitou via CLI")
	}
}

// 3) Duas adições simultâneas de processos diferentes: ambas sobrevivem.
func TestDuasEscritasConcorrentes(t *testing.T) {
	s1, s2 := loadPair(t)
	s1.appendEntry(entryFor("a1"))
	s2.appendEntry(entryFor("a2"))
	if err := s1.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}
	disk := manifestByUID(s1.Cfg.Root)
	if !disk["uid-a1"] || !disk["uid-a2"] {
		t.Fatalf("adições concorrentes se perderam: %v", disk)
	}
}

// 4) Update local (rename) vence quando o disco mudou também.
func TestUpdateLocalVenceNoMerge(t *testing.T) {
	s1, _ := loadPair(t)
	s1.appendEntry(entryFor("mover"))
	if err := s1.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s1.MoveEntry("uid-mover", "/sub"); err != nil {
		if err := s1.CreateFolder("/sub"); err != nil {
			t.Fatal(err)
		}
		if err := s1.MoveEntry("uid-mover", "/sub"); err != nil {
			t.Fatal(err)
		}
	}
	e := s1.EntriesSnapshot()[0]
	if e.Path != "/sub/mover" {
		t.Fatalf("move não aplicou: %s", e.Path)
	}
}
