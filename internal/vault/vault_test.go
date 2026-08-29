package vault

import (
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB",
		1048576: "1.0 MB", 5505000: "5.2 MB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func fakeStore() *Store {
	return &Store{
		Man: &Manifest{Version: manifestVersion},
		timeNow: func() time.Time { return time.Unix(0, 0) },
	}
}

func TestMatchPriorizaExato(t *testing.T) {
	s := fakeStore()
	s.Man.Entries = []*Entry{
		{ID: "abcdef1234567890", Name: "relatorio.pdf", Group: "docs", RelPath: "relatorio.pdf"},
		{ID: "ffffffffffffffff", Name: "relatorio_final.pdf", Group: "docs2", RelPath: "x/relatorio_final.pdf"},
	}
	got := s.Match("relatorio.pdf")
	if len(got) != 1 || got[0].Name != "relatorio.pdf" {
		t.Fatalf("esperava match exato único, obtive %v", got)
	}
	if got := s.Match("abc"); len(got) != 1 || got[0].ID[:3] != "abc" {
		t.Fatalf("prefixo de id deveria casar: %v", got)
	}
	if got := s.Match("docs"); len(got) != 2 {
		t.Fatalf("substring de grupo deveria casar ambos: %v", got)
	}
	if got := s.Match("inexistente"); len(got) != 0 {
		t.Fatalf("nada deveria casar: %v", got)
	}
}

func TestComputeStatsDedup(t *testing.T) {
	s := fakeStore()
	s.Man.Entries = []*Entry{
		{ID: "h1", Size: 1000, CromSize: 100, CromFile: "h1.crom"},
		{ID: "h1", Size: 1000, CromSize: 100, CromFile: "h1.crom"}, // dedup
		{ID: "h2", Size: 500, CromSize: 250, CromFile: "h2.crom"},
	}
	st := s.ComputeStats()
	if st.Files != 3 {
		t.Errorf("Files = %d, want 3", st.Files)
	}
	if st.Unique != 2 {
		t.Errorf("Unique = %d, want 2", st.Unique)
	}
	if st.Original != 2500 {
		t.Errorf("Original = %d, want 2500", st.Original)
	}
	if st.DedupSaved != 1000 {
		t.Errorf("DedupSaved = %d, want 1000", st.DedupSaved)
	}
	if st.Stored != 350 {
		t.Errorf("Stored = %d, want 350", st.Stored)
	}
}

func TestExpandHome(t *testing.T) {
	if !strings.HasPrefix(ExpandHome("~"), "/") {
		t.Fatal("~/ deveria expandir para caminho absoluto")
	}
	if got := ExpandHome("/tmp/x"); got != "/tmp/x" {
		t.Fatalf("caminho absoluto não deveria mudar: %q", got)
	}
}
