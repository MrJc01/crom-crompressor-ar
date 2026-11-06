package vault

import (
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
