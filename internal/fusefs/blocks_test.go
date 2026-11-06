package fusefs

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

// Testes do caminho de leitura/escrita SEM montar (o FUSE não monta em todo
// ambiente): chamam o filesystem direto, como o kernel chamaria.

func newVaultFS(t *testing.T) (*VaultFS, *vault.Store, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "crompressor-falso")
	os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755) // o depósito não usa o motor
	t.Setenv("CROMPRESSOR_BIN", bin)
	st, err := vault.LoadStore(filepath.Join(root, "cofre"))
	if err != nil {
		t.Fatal(err)
	}
	return New(st, Options{}), st, root
}

func texto(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	const abc = "abcdefghij klmnop\nqrstuv wxyz "
	for i := range b {
		b[i] = abc[r.Intn(len(abc))]
	}
	return b
}

func readAt(t *testing.T, f nodefs.File, off int64, n int) []byte {
	t.Helper()
	res, st := f.Read(make([]byte, n), off)
	if st != fuse.OK {
		t.Fatalf("Read(%d,%d): %v", off, n, st)
	}
	b, _ := res.Bytes(make([]byte, n))
	return b
}

func spoolFiles(t *testing.T, v *VaultFS) int {
	ents, _ := os.ReadDir(v.opt.SpoolDir)
	return len(ents)
}

func TestLeituraSobDemandaSemSpool(t *testing.T) {
	v, st, root := newVaultFS(t)
	data := texto(5<<20, 1)
	src := filepath.Join(root, "grande.txt")
	os.WriteFile(src, data, 0o644)
	if _, err := st.Add(src, vault.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	e := st.EntryByPath("/grande.txt")
	if e == nil || !e.IsBlocks() {
		t.Fatalf("entrada não foi para o depósito: %+v", e)
	}
	f, s := v.Open("/grande.txt", uint32(os.O_RDONLY), nil)
	if s != fuse.OK {
		t.Fatalf("Open: %v", s)
	}
	defer f.Release()
	for _, off := range []int64{0, 1, 4 << 20, int64(len(data)) - 10, 123457} {
		got := readAt(t, f, off, 128<<10)
		want := data[off:min(int(off)+128<<10, len(data))]
		if !bytes.Equal(got, want) {
			t.Fatalf("Read em %d errado", off)
		}
	}
	var attr fuse.Attr
	f.GetAttr(&attr)
	if attr.Size != uint64(len(data)) {
		t.Fatalf("tamanho %d ≠ %d", attr.Size, len(data))
	}
	if n := spoolFiles(t, v); n != 0 {
		t.Fatalf("leitura criou %d arquivo(s) no spool (deveria ler sob demanda)", n)
	}
}

func TestEscritaPelaMontagemVaiParaODeposito(t *testing.T) {
	v, st, _ := newVaultFS(t)
	data := texto(300<<10, 2)
	f, s := v.Create("/novo.txt", uint32(os.O_WRONLY|os.O_CREATE), 0o644, nil)
	if s != fuse.OK {
		t.Fatalf("Create: %v", s)
	}
	if n, s := f.Write(data, 0); s != fuse.OK || int(n) != len(data) {
		t.Fatalf("Write: %v", s)
	}
	f.Flush()
	f.Release()
	deadline := time.Now().Add(10 * time.Second)
	var e *vault.Entry
	for time.Now().Before(deadline) {
		v.tryImportPending("/novo.txt")
		if e = st.EntryByPath("/novo.txt"); e != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if e == nil || !e.IsBlocks() {
		t.Fatalf("escrita não importada no depósito: %+v", e)
	}
	time.Sleep(200 * time.Millisecond)
	if n := spoolFiles(t, v); n != 0 {
		t.Fatalf("depois de importar sobraram %d arquivo(s) crus no spool", n)
	}
	r, s := v.Open("/novo.txt", uint32(syscall.O_RDONLY), nil)
	if s != fuse.OK {
		t.Fatal(s)
	}
	defer r.Release()
	if !bytes.Equal(readAt(t, r, 0, len(data)+10), data) {
		t.Fatal("conteúdo lido difere do escrito")
	}
}

// Truncar um arquivo não pode alterar outro com o mesmo conteúdo (dedup).
func TestTruncateNaoEstragaCopiaDeduplicada(t *testing.T) {
	v, st, root := newVaultFS(t)
	data := texto(200<<10, 3)
	for _, n := range []string{"a.txt", "b.txt"} {
		p := filepath.Join(root, n)
		os.WriteFile(p, data, 0o644)
		if _, err := st.Add(p, vault.AddOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if s := v.Truncate("/a.txt", 1000, nil); s != fuse.OK {
		t.Fatalf("Truncate: %v", s)
	}
	if a := st.EntryByPath("/a.txt"); a == nil || a.Size != 1000 {
		t.Fatalf("a.txt não truncou: %+v", a)
	}
	b := st.EntryByPath("/b.txt")
	f, s := v.Open("/b.txt", uint32(os.O_RDONLY), nil)
	if s != fuse.OK || b.Size != int64(len(data)) {
		t.Fatalf("b.txt mudou: %+v", b)
	}
	defer f.Release()
	if !bytes.Equal(readAt(t, f, 0, len(data)), data) {
		t.Fatal("conteúdo de b.txt foi alterado pelo truncate de a.txt")
	}
}

// Apagar a última entrada de um conteúdo libera o disco.
func TestUnlinkLiberaDeposito(t *testing.T) {
	v, st, root := newVaultFS(t)
	data := make([]byte, 6<<20)
	rand.New(rand.NewSource(4)).Read(data)
	p := filepath.Join(root, "aleatorio.bin")
	os.WriteFile(p, data, 0o644)
	if _, err := st.Add(p, vault.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	before := st.ComputeStats().StoreOnDisk
	if s := v.Unlink("/aleatorio.bin", nil); s != fuse.OK {
		t.Fatalf("Unlink: %v", s)
	}
	after := st.ComputeStats().StoreOnDisk
	if after > before-int64(len(data))*9/10 {
		t.Fatalf("disco não foi liberado: %d → %d", before, after)
	}
}
