package bundle

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

// Regressão (06/11/2026): o import de .cromar usava Fuzziness>0, que no motor
// novo liga o "clone variacional" e trocava trechos entre os arquivos.
// Precisa do motor em bin/crompressor (make build).
func TestExportImportRoundtrip(t *testing.T) {
	bin, _ := filepath.Abs("../../bin/crompressor")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sem bin/crompressor (rode make build)")
	}
	t.Setenv("CROMPRESSOR_BIN", bin)
	root := t.TempDir()
	src := filepath.Join(root, "pk")
	os.MkdirAll(src, 0o755)
	r := rand.New(rand.NewSource(1))
	words := []string{"cofre", "bloco", "versão", "disco", "livro", "capítulo", "frase", "texto"}
	want := map[string][]byte{}
	for i := 0; i < 6; i++ { // textos parecidos: o cérebro do pacote acha trechos comuns
		var b bytes.Buffer
		for b.Len() < 200<<10 {
			fmt.Fprintf(&b, "%s %s %d\n", words[r.Intn(len(words))], words[r.Intn(len(words))], r.Intn(1000))
		}
		name := fmt.Sprintf("t%d.txt", i)
		want[name] = b.Bytes()
		os.WriteFile(filepath.Join(src, name), b.Bytes(), 0o644)
	}

	a, err := vault.LoadStore(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add(src, vault.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "pk.cromar")
	if _, err := Export(a, "pk", out, "", false); err != nil {
		t.Fatal(err)
	}

	b, err := vault.LoadStore(filepath.Join(root, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Import(b, out, ""); err != nil || n != len(want) {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
	dst := filepath.Join(root, "ext")
	res, err := b.Extract(b.Match("pk"), vault.ExtractOptions{OutDir: dst})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range res {
		if !x.Ok {
			t.Fatalf("extract %s: %s", x.Name, x.Err)
		}
	}
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(dst, "pk", name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s voltou diferente do original", name)
		}
	}
}
