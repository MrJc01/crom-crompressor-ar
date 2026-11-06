package store

import (
	"bytes"
	"math/rand"
	"testing"
)

// Frame que começa com dado aleatório (jpg, vídeo) mas termina com texto
// precisa ser comprimido; frame todo aleatório fica cru.
func TestAmostraVariosPontos(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rnd := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(rnd)
	if s.worthCompressing(rnd) {
		t.Fatal("aleatório não deveria valer a pena")
	}
	misto := append(append([]byte{}, rnd[:512<<10]...), bytes.Repeat([]byte("linha de texto bem repetida\n"), 20000)...)
	if !s.worthCompressing(misto[:1<<20]) {
		t.Fatal("frame com texto no fim deveria ser comprimido")
	}
	j := &frameJob{raw: misto[:1<<20]}
	s.compressBatch([]*frameJob{j})
	if j.codec != codecZstd || len(j.data) > 600<<10 {
		t.Fatalf("codec %d, %d bytes", j.codec, len(j.data))
	}
}
