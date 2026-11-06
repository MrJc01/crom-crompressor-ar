package store

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

func readAllStore(t *testing.T, s *Store, h Hash) []byte {
	t.Helper()
	f, err := s.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	if _, err := f.WriteTo(&w); err != nil {
		t.Fatal(err)
	}
	// ReadAt em pedaços tortos também tem de bater
	got := make([]byte, 0, f.Size())
	p := make([]byte, 777)
	for off := int64(0); off < f.Size(); {
		n, err := f.ReadAt(p, off)
		got = append(got, p[:n]...)
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, w.Bytes()) {
		t.Fatal("ReadAt e WriteTo diferem")
	}
	return w.Bytes()
}

// Objeto solto do git: zlib nível 1 de "blob N\0" + conteúdo. O cofre guarda
// descomprimido (deduplica com o arquivo de verdade) e devolve os bytes exatos.
func TestZlibTransparente(t *testing.T) {
	if !haveZlib {
		t.Skip("sem cgo")
	}
	dir := t.TempDir()
	s := openT(t, dir)
	plain := make([]byte, 3<<20)
	rand.New(rand.NewSource(7)).Read(plain)
	obj := append([]byte(fmt.Sprintf("blob %d\x00", len(plain))), plain...)
	git, err := zlibCompress(obj, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bytes.NewReader(plain)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Put(bytes.NewReader(git))
	if err != nil {
		t.Fatal(err)
	}
	if res.Hash != Hash(sha256.Sum256(git)) || res.Size != int64(len(git)) {
		t.Fatalf("hash/tamanho devem ser do arquivo original")
	}
	if rr, _ := s.Recipe(res.Hash); rr != nil {
		t.Logf("xform=%d chunks=%d novos=%d/%d", rr.Xform, len(rr.Chunks), res.NewChunks, res.Chunks)
	}
	if res.NewBytes > 256<<10 {
		t.Fatalf("objeto do git deveria deduplicar com o arquivo: %d bytes novos", res.NewBytes)
	}
	r, _ := s.Recipe(res.Hash)
	if r.Xform != xformZlib || r.Level != 1 {
		t.Fatalf("receita sem transformação: %+v", r.Xform)
	}
	if !bytes.Equal(readAllStore(t, s, res.Hash), git) {
		t.Fatal("bytes diferentes do original")
	}
	// reabrir o depósito: a receita v2 vem do disco
	s.Close()
	s2 := openT(t, dir)
	if !bytes.Equal(readAllStore(t, s2, res.Hash), git) {
		t.Fatal("bytes diferentes depois de reabrir")
	}
	if err := s2.Verify(res.Hash); err != nil {
		t.Fatal(err)
	}
}

// zlib que a biblioteca do sistema não recria (outro compressor) e lixo com
// cabeçalho de zlib: vão como estão, sem erro.
func TestZlibNaoRecriavelFicaComoEsta(t *testing.T) {
	s := openT(t, t.TempDir())
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, 9) // compress/zlib do Go: bytes diferentes da zlib C
	w.Write(textData(3, 1<<20))
	w.Close()
	casos := [][]byte{b.Bytes(), {0x78, 0x9c, 1, 2, 3}, {0x78, 0x01}, append(append([]byte{}, b.Bytes()...), "lixo no fim"...)}
	for i, c := range casos {
		res, err := s.Put(bytes.NewReader(c))
		if err != nil {
			t.Fatal(i, err)
		}
		if !bytes.Equal(readAllStore(t, s, res.Hash), c) {
			t.Fatal(i, "conteúdo diferente")
		}
	}
}

func TestReceitaV2IdaEVolta(t *testing.T) {
	r := &Recipe{Hash: Hash{1}, Size: 10, Xform: xformZlib, Level: 6, Chunks: []RecipeChunk{{Hash: Hash{2}, Len: 40}}}
	got, err := decodeRecipe(r.Hash, encodeRecipe(r))
	if err != nil || got.Xform != xformZlib || got.Level != 6 || got.Size != 10 || got.Chunks[0].Len != 40 {
		t.Fatalf("%+v %v", got, err)
	}
}
