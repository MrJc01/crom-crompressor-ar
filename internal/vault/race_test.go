package vault

import (
	"fmt"
	"sync"
	"testing"
)

// Regressão da Store compartilhada (28/08): FUSE grava (append) enquanto
// leitores (painel web) tiram snapshots e agregam — execute com -race.
func TestMutacaoConcorrenteComLeitores(t *testing.T) {
	s := fakeStoreComRoot(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // produtor (papel do FUSE importando)
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.appendEntry(&Entry{UID: fmt.Sprint(i), ID: "h", Name: "a", RelPath: "a", Path: "/a" + fmt.Sprint(i), CromFile: "x.crom", Size: 1})
			s.appendFolder("/p" + fmt.Sprint(i%7))
		}
	}()
	go func() { // leitores (papel do painel web)
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_ = s.EntriesSnapshot()
			_ = s.FoldersSnapshot()
			_ = s.ComputeStats()
			_ = s.ComputeBrainStats()
		}
	}()
	wg.Wait()
	if got := len(s.EntriesSnapshot()); got != 500 {
		t.Fatalf("entradas = %d, want 500", got)
	}
}
