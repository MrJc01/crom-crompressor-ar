package vault

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompactaEmOcioso(t *testing.T) {
	s, root := newTestStore(t)
	src := filepath.Join(root, "proj")
	files := writeTree(t, src, 200, 7)
	if _, err := s.Add(src, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(s.StoreDir(), "index.log")
	ctx := context.Background()

	// escrita recente: não mexe
	saved, more, err := s.CompactIdleOnce(ctx, IdleOptions{Idle: func() bool { return true }})
	if err != nil || saved != 0 || !more {
		t.Fatalf("com escrita recente: saved=%d more=%v err=%v", saved, more, err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(idx, old, old)

	// PC ocupado: não mexe
	saved, more, err = s.CompactIdleOnce(ctx, IdleOptions{Idle: func() bool { return false }})
	if err != nil || saved != 0 || !more {
		t.Fatalf("PC ocupado: saved=%d more=%v err=%v", saved, more, err)
	}

	// parado: recomprime tudo, em rodadas pequenas
	saved, more, err = s.CompactIdleOnce(ctx, IdleOptions{Idle: func() bool { return true }, RoundSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if more || saved <= 0 {
		t.Fatalf("parado: saved=%d more=%v", saved, more)
	}
	out := filepath.Join(root, "out")
	xr, err := s.Extract(s.EntriesSnapshot(), ExtractOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range xr {
		if !x.Ok {
			t.Fatalf("extract %s: %s", x.Name, x.Err)
		}
	}
	for rel, data := range files {
		got, err := os.ReadFile(filepath.Join(out, "proj", rel))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s voltou diferente", rel)
		}
	}
}
