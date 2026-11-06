package vault

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "crompressor-falso")
	os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("CROMPRESSOR_BIN", bin)
	s, err := LoadStore(filepath.Join(root, "cofre"))
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func writeTree(t *testing.T, dir string, n int, seed int64) map[string][]byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	files := map[string][]byte{}
	for i := 0; i < n; i++ {
		var b bytes.Buffer
		for b.Len() < 500+r.Intn(20000) {
			fmt.Fprintf(&b, "linha %d do arquivo %d com texto repetido\n", r.Intn(50), i%17)
		}
		rel := filepath.Join(fmt.Sprintf("sub%d", i%5), fmt.Sprintf("f%03d.txt", i))
		files[rel] = b.Bytes()
	}
	files["dup-a.bin"] = bytes.Repeat([]byte("igual"), 9000)
	files["sub1/dup-b.bin"] = bytes.Repeat([]byte("igual"), 9000)
	for rel, data := range files {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
	}
	return files
}

func TestAddPastaEmLote(t *testing.T) {
	s, root := newTestStore(t)
	src := filepath.Join(root, "proj")
	files := writeTree(t, src, 300, 1)
	res, err := s.Add(src, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(files) || len(s.EntriesSnapshot()) != len(files) {
		t.Fatalf("esperava %d entradas, veio %d (resultados %d)", len(files), len(s.EntriesSnapshot()), len(res))
	}
	for _, e := range s.EntriesSnapshot() {
		if !e.IsBlocks() || !e.Verified {
			t.Fatalf("entrada %s: storage=%q verified=%v", e.RelPath, e.Storage, e.Verified)
		}
	}
	out := filepath.Join(root, "out")
	xr, err := s.Extract(s.EntriesSnapshot(), ExtractOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range xr {
		if !x.Ok {
			t.Fatalf("extract %s: %s", x.Name, x.Err)
		}
	}
	for rel, data := range files {
		got, err := os.ReadFile(filepath.Join(out, "proj", rel))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s voltou diferente", rel)
		}
	}
	// segunda importação da mesma pasta: nada novo no disco
	before := s.ComputeStats().StoreOnDisk
	if _, err := s.Add(src, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if after := s.ComputeStats().StoreOnDisk; after > before+4096 {
		t.Fatalf("reimportar a mesma pasta cresceu o disco: %d → %d", before, after)
	}
	// o cofre reaberto (outro processo) enxerga tudo
	s2, err := LoadStore(s.Cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s2.EntriesSnapshot() {
		if err := s2.VerifyEntry(e, ""); err != nil {
			t.Fatalf("cofre reaberto: %s: %v", e.RelPath, err)
		}
	}
}

// Gravar e apagar pelo cofre (como o FUSE faz) durante um Add grande não
// pode travar nem perder dado.
func TestImportEApagarDuranteAddSemTravar(t *testing.T) {
	s, root := newTestStore(t)
	src := filepath.Join(root, "grande")
	writeTree(t, src, 600, 2)
	solo := filepath.Join(root, "solo.txt")
	os.WriteFile(solo, []byte("arquivo gravado pela montagem durante o import"), 0o644)
	velho := filepath.Join(root, "velho.txt")
	os.WriteFile(velho, bytes.Repeat([]byte("apagar "), 5000), 0o644)
	if _, err := s.Add(velho, AddOptions{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, err := s.Add(src, AddOptions{}); done <- err }()
	time.Sleep(50 * time.Millisecond)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := s.ImportFile(AddFileOpts{Src: solo, Folder: "/", Name: "solo.txt"})
		done <- err
	}()
	go func() {
		defer wg.Done()
		_, err := s.RemoveByPath("/velho.txt")
		done <- err
	}()
	fin := make(chan struct{})
	go func() { wg.Wait(); close(fin) }()
	select {
	case <-fin:
	case <-time.After(60 * time.Second):
		t.Fatal("travou: Add em lote × import/remoção concorrentes")
	}
	close(done)
	for err := range done {
		if err != nil {
			t.Fatal(err)
		}
	}
	e := s.EntryByPath("/solo.txt")
	if e == nil {
		t.Fatal("import concorrente sumiu")
	}
	if err := s.VerifyEntry(e, ""); err != nil {
		t.Fatalf("import concorrente ilegível: %v", err)
	}
	if s.EntryByPath("/velho.txt") != nil {
		t.Fatal("remoção concorrente não valeu")
	}
}
