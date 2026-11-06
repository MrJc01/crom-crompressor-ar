package store

import (
	"bytes"
	"os"
	"testing"
)

// Apagar a versão antiga não pode deixar a base inteira no disco só por
// causa dos deltas da nova: com um delta por base, guardar a nova inteira
// ocupa menos.
func TestGCTiraBaseOrfa(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	a := randData(51, 2<<20)
	b := quaseIgual(a, 3000)
	ra := put(t, s, a)
	rb := put(t, s, b)
	if err := s.Remove(ra.Hash); err != nil {
		t.Fatal(err)
	}
	g, err := s.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Optimize.OrphanBases == 0 {
		t.Fatalf("nenhuma base órfã resolvida: %+v", g.Optimize)
	}
	if _, err := s.GC(0); err != nil { // segunda passada leva os deltas mortos e as bases
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, s, rb.Hash), b) {
		t.Fatal("conteúdo errado")
	}
	if err := s.Verify(rb.Hash); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	r, _ := s.Recipe(rb.Hash)
	if st.UniqueChunks != len(r.Chunks) {
		t.Fatalf("sobraram %d blocos para %d da receita", st.UniqueChunks, len(r.Chunks))
	}
	s.Close()
	if !bytes.Equal(readAll(t, openT(t, dir), rb.Hash), b) {
		t.Fatal("conteúdo errado depois de reabrir")
	}
}

// Com uma terceira versão viva, os deltas da segunda trocam de base para ela.
func TestGCTrocaBase(t *testing.T) {
	s := openT(t, t.TempDir())
	a := randData(52, 1<<20)
	b := quaseIgual(a, 3000)
	ra := put(t, s, a)
	rb := put(t, s, b)
	c := quaseIgual(a, 4100)
	s.opt.NoDelta = true
	rc := put(t, s, c) // c inteiro: candidato a nova base
	s.opt.NoDelta = false
	if _, err := s.Optimize(); err != nil { // registra as features de c
		t.Fatal(err)
	}
	if err := s.Remove(ra.Hash); err != nil {
		t.Fatal(err)
	}
	g, err := s.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Optimize.Rebased == 0 {
		t.Fatalf("nenhum delta trocou de base: %+v", g.Optimize)
	}
	s.GC(0)
	for _, x := range []struct {
		h Hash
		d []byte
	}{{rb.Hash, b}, {rc.Hash, c}} {
		if !bytes.Equal(readAll(t, s, x.h), x.d) {
			t.Fatal("conteúdo errado")
		}
	}
}

// Blocos gravados sem delta (depósito antigo) viram delta na arrumação.
func TestOptimizeBlocosAntigos(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{FrameSize: 256 << 10, NoDelta: true})
	if err != nil {
		t.Fatal(err)
	}
	a := randData(53, 2<<20)
	b := quaseIgual(a, 3000)
	ra := put(t, s, a)
	rb := put(t, s, b)
	s.Close()
	s = openT(t, dir)
	antes, _ := s.Stats()
	o, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if o.NewDeltas < o.Scanned/3 {
		t.Fatalf("poucos deltas: %+v", o)
	}
	if _, err := s.GC(0); err != nil {
		t.Fatal(err)
	}
	depois, _ := s.Stats()
	if depois.DiskBytes > antes.DiskBytes*7/10 {
		t.Fatalf("disco %d → %d", antes.DiskBytes, depois.DiskBytes)
	}
	if !bytes.Equal(readAll(t, s, ra.Hash), a) || !bytes.Equal(readAll(t, s, rb.Hash), b) {
		t.Fatal("conteúdo errado")
	}
	// segunda arrumação não analisa de novo
	if o2, _ := s.Optimize(); o2.Scanned != 0 {
		t.Fatalf("analisou de novo: %+v", o2)
	}
}

func TestGCCompactaFeats(t *testing.T) {
	s := openT(t, t.TempDir())
	var hs []Hash
	for i := int64(0); i < 4; i++ {
		hs = append(hs, put(t, s, randData(60+i, 1<<20)).Hash)
	}
	fi, _ := os.Stat(s.featPath())
	for _, h := range hs[:3] {
		s.Remove(h)
	}
	if _, err := s.GC(0); err != nil {
		t.Fatal(err)
	}
	fj, _ := os.Stat(s.featPath())
	if fj.Size()*2 > fi.Size() {
		t.Fatalf("feats.log %d → %d", fi.Size(), fj.Size())
	}
	// e o depósito continua achando parecidos
	r := put(t, s, quaseIgual(randData(63, 1<<20), 3000))
	if r.DeltaChunks < r.NewChunks*8/10 {
		t.Fatalf("depois da compactação: %d deltas de %d", r.DeltaChunks, r.NewChunks)
	}
}
