package vault

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Cofre do formato anterior (manifest.json cru) abre normalmente e, no
// primeiro Save, passa para manifest.json.zst e apaga o JSON velho.
func TestManifestComprimidoMigraDoJSON(t *testing.T) {
	s, root := newTestStore(t)
	src := filepath.Join(root, "proj")
	files := writeTree(t, src, 50, 3)
	if _, err := s.Add(src, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	vroot := s.Cfg.Root
	zst := ManifestPath(vroot)
	b, err := os.ReadFile(zst)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 4 || string(b[:4]) != string(zstdMagic) {
		t.Fatal("manifest não está comprimido")
	}
	raw, _ := json.Marshal(s.Man)
	if len(b)*3 > len(raw) {
		t.Fatalf("compressão fraca: %d → %d", len(raw), len(b))
	}

	// volta ao formato antigo: só manifest.json
	os.WriteFile(filepath.Join(vroot, legacyManifest), raw, 0o644)
	os.Remove(zst)
	os.Remove(zst + ".bak")
	s2, err := LoadStore(vroot)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s2.EntriesSnapshot()); n != len(files) {
		t.Fatalf("formato antigo: %d entradas, esperava %d", n, len(files))
	}
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(vroot, legacyManifest)) {
		t.Fatal("manifest.json antigo ficou depois de migrar")
	}
	s3, err := LoadStore(vroot)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s3.EntriesSnapshot()); n != len(files) {
		t.Fatalf("após migrar: %d entradas, esperava %d", n, len(files))
	}
}

// O manifest gravado e lido em fluxo é idêntico ao json.Marshal do mesmo
// valor, inclusive com lista vazia/nula e campo desconhecido (versão futura).
func TestManifestEmFluxoIgualAoMarshal(t *testing.T) {
	m := &Manifest{Version: 3, Folders: []string{"/", "/a"}, Entries: []*Entry{
		{UID: "u1", ID: "x", Name: "a.txt", Size: 10, Storage: "blocks"},
		{UID: "u2", ID: "y", Name: "b \"c\".txt", Size: 0},
	}}
	for _, mm := range []*Manifest{m, {Version: 1}, {Version: 1, Entries: []*Entry{}}} {
		var got bytes.Buffer
		if err := encodeManifest(&got, mm); err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(mm)
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("fluxo difere do Marshal:\n%s\n%s", got.Bytes(), want)
		}
		p := filepath.Join(t.TempDir(), "m.json.zst")
		if err := writeManifest(p, mm); err != nil {
			t.Fatal(err)
		}
		back, err := loadManifest(p)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := json.Marshal(back)
		if !bytes.Equal(b2, want) && !(len(mm.Entries) == 0 && len(back.Entries) == 0) {
			t.Fatalf("ida e volta difere:\n%s\n%s", b2, want)
		}
	}
	// campo desconhecido é ignorado, JSON cru (legado) também abre
	p := filepath.Join(t.TempDir(), "manifest.json")
	os.WriteFile(p, []byte(`{"version":2,"futuro":{"x":[1,2]},"folders":["/"],"entries":[{"uid":"z","name":"n"}]}`), 0o644)
	back, err := loadManifest(p)
	if err != nil || len(back.Entries) != 1 || back.Entries[0].UID != "z" || back.Version != 2 {
		t.Fatalf("legado: %v %+v", err, back)
	}
	// lixo no fim não passa
	os.WriteFile(p, []byte(`{"version":2,"entries":[{"uid":"z"}`), 0o644)
	if _, err := loadManifest(p); err == nil {
		t.Fatal("manifest cortado deveria falhar")
	}
}
