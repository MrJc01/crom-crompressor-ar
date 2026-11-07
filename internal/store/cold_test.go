package store

import (
	"bytes"
	"testing"
)

func TestRecompressFrios(t *testing.T) {
	if !haveZstdMax {
		t.Skip("sem cgo")
	}
	dir := t.TempDir()
	s := openT(t, dir)
	var rs []PutResult
	var ds [][]byte
	for i := int64(0); i < 4; i++ {
		d := textData(70+i, 1<<20)
		ds = append(ds, d)
		rs = append(rs, put(t, s, d))
	}
	antes, _ := s.Stats()
	total := 0
	for {
		r, err := s.Recompress(1 << 20) // rodadas pequenas: várias passadas
		if err != nil {
			t.Fatal(err)
		}
		total += r.Frames
		if r.Remaining == 0 {
			break
		}
	}
	if total == 0 {
		t.Fatal("nenhum frame recomprimido")
	}
	depois, _ := s.Stats()
	if depois.DiskBytes >= antes.DiskBytes {
		t.Fatalf("disco %d → %d", antes.DiskBytes, depois.DiskBytes)
	}
	if r, _ := s.Recompress(1 << 30); r.Frames != 0 {
		t.Fatalf("recomprimiu de novo: %+v", r)
	}
	// gravar depois, GC e reabrir continuam funcionando
	more := quaseIgual(ds[0], 3000)
	rm := put(t, s, more)
	s.Remove(rs[1].Hash)
	if _, err := s.GC(0); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openT(t, dir)
	for i, r := range rs {
		if i == 1 {
			continue
		}
		if !bytes.Equal(readAll(t, s, r.Hash), ds[i]) {
			t.Fatal("conteúdo errado")
		}
		if err := s.Verify(r.Hash); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(readAll(t, s, rm.Hash), more) {
		t.Fatal("conteúdo novo errado")
	}
	t.Logf("disco %d → %d (%d frames)", antes.DiskBytes, depois.DiskBytes, total)
}
