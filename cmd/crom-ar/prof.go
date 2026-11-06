package main

import (
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
)

// Limite suave de memória: o coletor de lixo do Go deixa o heap dobrar antes
// de coletar, o que num PC com pouca RAM custa centenas de MB à toa. Com o
// limite ele coleta mais cedo perto de 384 MB (pode passar, nunca falha por
// isso). GOMEMLIMIT no ambiente troca o valor.
func init() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(384 << 20)
	}
	if p := os.Getenv("CROM_AR_CPUPROF"); p != "" {
		if f, err := os.Create(p); err == nil {
			pprof.StartCPUProfile(f)
		}
	}
}

// CROM_AR_MEMPROF=arquivo grava, ao sair, o perfil de memória (alocações
// acumuladas e em uso) para `go tool pprof`. Só para medir.
// CROM_AR_CPUPROF=arquivo grava o perfil de CPU do comando inteiro.
func writeMemProfile() {
	pprof.StopCPUProfile()
	p := os.Getenv("CROM_AR_MEMPROF")
	if p == "" {
		return
	}
	f, err := os.Create(p)
	if err != nil {
		return
	}
	defer f.Close()
	runtime.GC()
	pprof.Lookup("allocs").WriteTo(f, 0)
}
