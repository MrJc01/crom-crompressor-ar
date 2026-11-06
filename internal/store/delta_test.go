package store

import (
	"bytes"
	"testing"
)

// quaseIgual: cópia de b com um byte trocado a cada passo bytes (todo bloco
// muda, a deduplicação exata não acha nada).
func quaseIgual(b []byte, passo int) []byte {
	c := append([]byte(nil), b...)
	for i := passo / 2; i < len(c); i += passo {
		c[i] ^= 0x5a
	}
	return c
}

func TestDeltaBlocoParecido(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	a := randData(31, 2<<20)
	b := quaseIgual(a, 3000)
	ra := put(t, s, a)
	antes, _ := s.Stats()
	rb := put(t, s, b)
	depois, _ := s.Stats()
	if rb.NewChunks == 0 || rb.DeltaChunks < rb.NewChunks*8/10 {
		t.Fatalf("poucos deltas: %d de %d blocos novos", rb.DeltaChunks, rb.NewChunks)
	}
	if cresc := depois.DiskBytes - antes.DiskBytes; cresc > int64(len(b))/10 {
		t.Fatalf("versão parecida gastou %d bytes (original %d)", cresc, len(b))
	}
	if !bytes.Equal(readAll(t, s, rb.Hash), b) || !bytes.Equal(readAll(t, s, ra.Hash), a) {
		t.Fatal("conteúdo errado")
	}
	if err := s.Verify(rb.Hash); err != nil {
		t.Fatal(err)
	}
	// leitura aleatória atravessando blocos em delta
	f, err := s.Open(rb.Hash)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 100000)
	if _, err := f.ReadAt(p, 777777); err != nil || !bytes.Equal(p, b[777777:877777]) {
		t.Fatalf("ReadAt errado (%v)", err)
	}
	s.Close()
	s2 := openT(t, dir)
	if !bytes.Equal(readAll(t, s2, rb.Hash), b) {
		t.Fatal("delta ilegível depois de reabrir")
	}
}

func TestDeltaEntreExecucoes(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	a := randData(32, 1<<20)
	put(t, s, a)
	s.Close()
	s2 := openT(t, dir) // features vêm do feats.log
	b := quaseIgual(a, 2500)
	rb := put(t, s2, b)
	if rb.DeltaChunks < rb.NewChunks*8/10 {
		t.Fatalf("feats.log não serviu: %d deltas de %d", rb.DeltaChunks, rb.NewChunks)
	}
	if !bytes.Equal(readAll(t, s2, rb.Hash), b) {
		t.Fatal("conteúdo errado")
	}
}

func TestGCMantemBaseDoDelta(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	a := randData(33, 2<<20)
	b := quaseIgual(a, 3000)
	ra := put(t, s, a)
	rb := put(t, s, b)
	if err := s.Remove(ra.Hash); err != nil {
		t.Fatal(err)
	}
	for _, minDead := range []float64{0, 0.3, 0} {
		if _, err := s.GC(minDead); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(readAll(t, s, rb.Hash), b) {
			t.Fatal("GC apagou a base de um delta vivo")
		}
	}
	// a versão nova sai também: agora as bases podem ir embora
	if err := s.Remove(rb.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(0); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	if st.UniqueChunks != 0 {
		t.Fatalf("sobraram %d blocos depois de apagar tudo", st.UniqueChunks)
	}
	// e gravar de novo funciona com features apontando para blocos que saíram
	rc := put(t, s, b)
	if !bytes.Equal(readAll(t, s, rc.Hash), b) {
		t.Fatal("conteúdo errado depois do GC")
	}
	s.Close()
	s2 := openT(t, dir)
	if !bytes.Equal(readAll(t, s2, rc.Hash), b) {
		t.Fatal("conteúdo errado depois de reabrir")
	}
}

func TestDeltaArquivoDeUmBloco(t *testing.T) {
	s := openT(t, t.TempDir())
	a := randData(34, 3500) // um bloco só (receita curta, sem lista de blocos)
	b := quaseIgual(a, 1000)
	put(t, s, a)
	rb := put(t, s, b)
	if rb.DeltaChunks != 1 {
		t.Fatalf("esperava 1 delta, veio %d", rb.DeltaChunks)
	}
	r, err := s.Recipe(rb.Hash)
	if err != nil || r.Size != int64(len(b)) {
		t.Fatalf("tamanho da receita %v (%v)", r, err)
	}
	if !bytes.Equal(readAll(t, s, rb.Hash), b) {
		t.Fatal("conteúdo errado")
	}
}

func TestDeltaDesligado(t *testing.T) {
	s, err := Open(t.TempDir(), Options{FrameSize: 256 << 10, NoDelta: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := randData(35, 1<<20)
	put(t, s, a)
	if rb := put(t, s, quaseIgual(a, 3000)); rb.DeltaChunks != 0 {
		t.Fatalf("NoDelta gravou %d deltas", rb.DeltaChunks)
	}
}

// Texto em blocos novos sem parecido não pode virar delta ruim.
func TestDeltaNaoAtrapalhaTexto(t *testing.T) {
	s := openT(t, t.TempDir())
	for i := int64(0); i < 4; i++ {
		d := textData(40+i, 1<<20)
		r := put(t, s, d)
		if !bytes.Equal(readAll(t, s, r.Hash), d) {
			t.Fatal("conteúdo errado")
		}
	}
}
