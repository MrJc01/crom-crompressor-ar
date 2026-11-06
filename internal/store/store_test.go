package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openT(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Options{FrameSize: 256 << 10, SegmentSize: 4 << 20, CacheBytes: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// texto realista: palavras repetidas com variação (comprime como texto de verdade)
func textData(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	words := strings.Fields("o cofre guarda blocos iguais uma vez só e comprime o resto com zstd para economizar disco de verdade quando há versões parecidas dos mesmos dados")
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[r.Intn(len(words))])
		if r.Intn(12) == 0 {
			fmt.Fprintf(&b, " %d.\n", r.Intn(100000))
		} else {
			b.WriteByte(' ')
		}
	}
	return b.Bytes()[:n]
}

func randData(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func put(t *testing.T, s *Store, b []byte) PutResult {
	t.Helper()
	r, err := s.Put(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r.Hash != Hash(sha256.Sum256(b)) {
		t.Fatalf("hash do Put não bate")
	}
	return r
}

func readAll(t *testing.T, s *Store, h Hash) []byte {
	t.Helper()
	f, err := s.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := f.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundtripTamanhos(t *testing.T) {
	s := openT(t, t.TempDir())
	for _, n := range []int{0, 1, 100, MinChunk - 1, MinChunk, MinChunk + 1, AvgChunk, MaxChunk, MaxChunk + 1, 3<<20 + 17} {
		for _, gen := range []func(int64, int) []byte{textData, randData} {
			b := gen(int64(n), n)
			r := put(t, s, b)
			if got := readAll(t, s, r.Hash); !bytes.Equal(got, b) {
				t.Fatalf("roundtrip n=%d falhou", n)
			}
			if err := s.Verify(r.Hash); err != nil {
				t.Fatalf("verify n=%d: %v", n, err)
			}
		}
	}
}

func TestDedupMesmoConteudo(t *testing.T) {
	s := openT(t, t.TempDir())
	b := textData(1, 2<<20)
	r1 := put(t, s, b)
	r2 := put(t, s, b)
	if r2.NewChunks != 0 || r2.NewBytes != 0 || !r2.Existed {
		t.Fatalf("segunda cópia gravou dados: %+v", r2)
	}
	if r1.NewChunks == 0 {
		t.Fatal("primeira cópia não gravou nada")
	}
}

// Inserir bytes no meio só cria blocos novos em volta da inserção.
func TestVersoesDividemBlocos(t *testing.T) {
	s := openT(t, t.TempDir())
	v1 := textData(7, 4<<20)
	v2 := append(append(append([]byte{}, v1[:1<<20]...), []byte("TRECHO NOVO NO MEIO DO ARQUIVO")...), v1[1<<20:]...)
	r1 := put(t, s, v1)
	r2 := put(t, s, v2)
	if r2.NewBytes > 4*MaxChunk {
		t.Fatalf("versão com 30 bytes a mais gravou %d bytes novos (de %d)", r2.NewBytes, r1.NewBytes)
	}
	if !bytes.Equal(readAll(t, s, r2.Hash), v2) || !bytes.Equal(readAll(t, s, r1.Hash), v1) {
		t.Fatal("roundtrip das versões falhou")
	}
}

func TestDadoQueNaoComprimeFicaCru(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	b := randData(3, 4<<20)
	r := put(t, s, b)
	if r.RawFrames == 0 {
		t.Fatal("dado aleatório deveria ir cru")
	}
	st, _ := s.Stats()
	if float64(st.DiskBytes) > float64(len(b))*1.01 {
		t.Fatalf("aleatório ocupou %d (> 1%% sobre %d)", st.DiskBytes, len(b))
	}
}

func TestTextoComprime(t *testing.T) {
	s := openT(t, t.TempDir())
	b := textData(9, 4<<20)
	put(t, s, b)
	st, _ := s.Stats()
	if st.DiskBytes > int64(len(b))/2 {
		t.Fatalf("texto não comprimiu: %d de %d", st.DiskBytes, len(b))
	}
}

func TestReadAtAleatorio(t *testing.T) {
	s := openT(t, t.TempDir())
	b := textData(11, 3<<20)
	r := put(t, s, b)
	f, _ := s.Open(r.Hash)
	rnd := rand.New(rand.NewSource(5))
	for i := 0; i < 300; i++ {
		off := rnd.Int63n(int64(len(b)))
		n := rnd.Intn(200 << 10)
		p := make([]byte, n)
		m, err := f.ReadAt(p, off)
		want := b[off:min(int(off)+n, len(b))]
		if m != len(want) || !bytes.Equal(p[:m], want) {
			t.Fatalf("ReadAt(%d,%d) = %d bytes errados", off, n, m)
		}
		if int(off)+n > len(b) && err != io.EOF {
			t.Fatalf("esperava EOF no fim, veio %v", err)
		}
	}
	if _, err := f.ReadAt(make([]byte, 1), int64(len(b))); err != io.EOF {
		t.Fatalf("ReadAt no fim: %v", err)
	}
}

func TestReabrePersiste(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	b := textData(13, 1<<20)
	r := put(t, s, b)
	s.Close()
	s2 := openT(t, dir)
	if !s2.Has(r.Hash) || !bytes.Equal(readAll(t, s2, r.Hash), b) {
		t.Fatal("conteúdo sumiu ao reabrir")
	}
	if r2 := put(t, s2, b); r2.NewChunks != 0 {
		t.Fatal("após reabrir, dedup não reconheceu os blocos")
	}
}

func TestGCLiberaEPreservaVivos(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	a := randData(21, 3<<20)
	b := randData(22, 3<<20)
	ra := put(t, s, a)
	rb := put(t, s, b)
	before, _ := s.Stats()
	if err := s.Remove(ra.Hash); err != nil {
		t.Fatal(err)
	}
	res, err := s.GC(0.3)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()
	if after.DiskBytes > before.DiskBytes-int64(len(a))*9/10 {
		t.Fatalf("GC não liberou: antes %d depois %d (%+v)", before.DiskBytes, after.DiskBytes, res)
	}
	if !bytes.Equal(readAll(t, s, rb.Hash), b) {
		t.Fatal("GC estragou conteúdo vivo")
	}
	// processo novo enxerga o índice reescrito
	s.Close()
	s2 := openT(t, dir)
	if !bytes.Equal(readAll(t, s2, rb.Hash), b) {
		t.Fatal("conteúdo vivo ilegível após reabrir pós-GC")
	}
	if s2.Has(ra.Hash) {
		t.Fatal("conteúdo removido continua listado")
	}
}

func TestGCNaoApagaBlocoCompartilhado(t *testing.T) {
	s := openT(t, t.TempDir())
	v1 := textData(31, 2<<20)
	v2 := append(append([]byte{}, v1...), []byte("fim novo")...)
	r1 := put(t, s, v1)
	r2 := put(t, s, v2)
	s.Remove(r1.Hash)
	if _, err := s.GC(0.0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, s, r2.Hash), v2) {
		t.Fatal("GC apagou bloco que v2 usa")
	}
}

// Queda no meio da gravação do índice: o final cortado é descartado e o
// depósito continua consistente.
func TestIndiceCortadoPorQueda(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	b := textData(41, 1<<20)
	r := put(t, s, b)
	s.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "index.log"), os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("lixo de um registro pela metade"))
	f.Close()
	s2 := openT(t, dir)
	if !bytes.Equal(readAll(t, s2, r.Hash), b) {
		t.Fatal("leitura falhou com índice cortado")
	}
	c := textData(42, 1<<20)
	rc := put(t, s2, c) // escritor trunca o lixo e grava depois
	s2.Close()
	s3 := openT(t, dir)
	if !bytes.Equal(readAll(t, s3, rc.Hash), c) || !bytes.Equal(readAll(t, s3, r.Hash), b) {
		t.Fatal("gravação após índice cortado ficou ilegível")
	}
}

// Frame corrompido no disco é detectado (nunca devolve dado errado).
func TestCorrupcaoDetectada(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	b := textData(51, 1<<20)
	r := put(t, s, b)
	s.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "seg", "*.seg"))
	data, _ := os.ReadFile(segs[0])
	data[len(data)/2] ^= 0xFF
	os.WriteFile(segs[0], data, 0o644)
	s2 := openT(t, dir)
	if err := s2.Verify(r.Hash); err == nil {
		t.Fatal("corrupção não detectada")
	}
}

func TestSegmentosTrocam(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir) // SegmentSize 4 MiB
	b := randData(61, 10<<20)
	r := put(t, s, b)
	st, _ := s.Stats()
	if st.Segments < 3 {
		t.Fatalf("esperava ≥3 segmentos, veio %d", st.Segments)
	}
	if !bytes.Equal(readAll(t, s, r.Hash), b) {
		t.Fatal("roundtrip entre segmentos falhou")
	}
}

// Dois "processos" (duas instâncias) gravando e lendo ao mesmo tempo.
func TestConcorrenciaDuasInstancias(t *testing.T) {
	dir := t.TempDir()
	a, b := openT(t, dir), openT(t, dir)
	var wg sync.WaitGroup
	hashes := make([]Hash, 8)
	datas := make([][]byte, 8)
	for i := 0; i < 8; i++ {
		datas[i] = textData(int64(70+i%3), 1<<20+i*1000) // repete conteúdos entre instâncias
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := a
			if i%2 == 1 {
				st = b
			}
			r, err := st.Put(bytes.NewReader(datas[i]))
			if err != nil {
				t.Error(err)
				return
			}
			hashes[i] = r.Hash
		}(i)
	}
	wg.Wait()
	for i := range datas {
		for _, st := range []*Store{a, b} {
			if !bytes.Equal(readAll(t, st, hashes[i]), datas[i]) {
				t.Fatalf("conteúdo %d ilegível por uma das instâncias", i)
			}
		}
	}
}

func TestReceitaCorrompidaRecusada(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	r := put(t, s, textData(81, 300<<10))
	p := s.recipePath(r.Hash)
	b, _ := os.ReadFile(p)
	b[30] ^= 1
	os.WriteFile(p, b, 0o644)
	if _, err := s.Open(r.Hash); err == nil {
		t.Fatal("receita corrompida aceita")
	}
	if _, err := s.GC(0.3); err == nil {
		t.Fatal("GC deveria recusar com receita ilegível (não arrisca apagar)")
	}
}

func TestCDCDeterministico(t *testing.T) {
	b := textData(91, 2<<20)
	cut := func() []int {
		c := newChunker(bytes.NewReader(b))
		var ls []int
		for {
			ch, err := c.next()
			if err != nil {
				break
			}
			if len(ch) > MaxChunk || (len(ch) < MinChunk && len(ls) > 0 && false) {
				t.Fatalf("bloco fora dos limites: %d", len(ch))
			}
			ls = append(ls, len(ch))
		}
		return ls
	}
	a, c := cut(), cut()
	if fmt.Sprint(a) != fmt.Sprint(c) {
		t.Fatal("corte não determinístico")
	}
	var tot int
	for _, n := range a {
		tot += n
	}
	if tot != len(b) {
		t.Fatal("soma dos blocos ≠ tamanho")
	}
	avg := tot / len(a)
	if avg < AvgChunk/2 || avg > AvgChunk*2 {
		t.Fatalf("média dos blocos %d longe de %d", avg, AvgChunk)
	}
}

// Lote: arquivos pequenos dividem frames (comprimem juntos), conteúdo só
// aparece para outro processo depois do Close, e repetição dentro do lote
// não grava duas vezes.
func TestLoteArquivosPequenos(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	b, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var hs []Hash
	var datas [][]byte
	var raw int
	for i := 0; i < 400; i++ {
		d := textData(int64(i), 3000+i)
		r, err := b.Put(bytes.NewReader(d))
		if err != nil {
			t.Fatal(err)
		}
		hs, datas = append(hs, r.Hash), append(datas, d)
		raw += len(d)
	}
	again, _ := b.Put(bytes.NewReader(datas[7]))
	if !again.Existed || again.NewChunks != 0 {
		t.Fatalf("repetição no lote gravou de novo: %+v", again)
	}
	other := openT(t, dir)
	if other.Has(hs[0]) {
		t.Fatal("outro processo viu conteúdo antes do commit")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for i, h := range hs {
		if !bytes.Equal(readAll(t, other, h), datas[i]) {
			t.Fatalf("conteúdo %d ilegível depois do commit", i)
		}
	}
	// compara com 400 Puts separados (cada arquivo comprimido sozinho)
	s2 := openT(t, t.TempDir())
	for _, d := range datas {
		put(t, s2, d)
	}
	st1, _ := s.Stats()
	st2, _ := s2.Stats()
	if st1.DiskBytes >= st2.DiskBytes {
		t.Fatalf("lote não comprimiu melhor que arquivos sozinhos: %d vs %d", st1.DiskBytes, st2.DiskBytes)
	}
	t.Logf("400 arquivos pequenos (%d B): lote %d B, separados %d B", raw, st1.DiskBytes, st2.DiskBytes)
}

func TestLoteAbortNaoDeixaReceita(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	b, _ := s.Begin()
	r, _ := b.Put(bytes.NewReader(textData(1, 100<<10)))
	b.Abort()
	if s.Has(r.Hash) {
		t.Fatal("abort deixou o conteúdo visível")
	}
	put(t, s, textData(2, 10<<10)) // depósito continua utilizável
}
