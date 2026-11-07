package vault

import (
	"bufio"
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Compactação automática no residente (item 62): quando o PC está parado,
// recomprime no zstd 19 os frames que o add gravou no nível rápido, em
// rodadas curtas (o depósito fica travado só durante uma rodada, então um
// add que chegue espera poucos segundos). Ler nunca é afetado.

// IdleOptions controla o laço de compactação em ocioso.
type IdleOptions struct {
	Every     time.Duration // intervalo entre checagens (padrão 5 min)
	Quiet     time.Duration // depósito sem escrita há pelo menos isso (padrão 10 min)
	RoundSize int64         // bytes de segmentos por rodada (padrão 64 MiB)
	MinFreeMB int64         // memória livre mínima (padrão 768 MiB)
	MaxLoad   float64       // load médio de 1 min por CPU (padrão 0.3)
	// Idle, se não nulo, substitui a checagem do sistema (testes).
	Idle func() bool
}

func (o *IdleOptions) defaults() {
	if o.Every <= 0 {
		o.Every = 5 * time.Minute
	}
	if o.Quiet <= 0 {
		o.Quiet = 10 * time.Minute
	}
	if o.RoundSize <= 0 {
		o.RoundSize = 64 << 20
	}
	if o.MinFreeMB <= 0 {
		o.MinFreeMB = 768
	}
	if o.MaxLoad <= 0 {
		o.MaxLoad = 0.3
	}
}

// systemIdle: CPU parada e memória sobrando (o PC tem pouca RAM e a
// recompressão usa umas centenas de MB).
func systemIdle(o IdleOptions) bool {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return false
	}
	load, err := strconv.ParseFloat(f[0], 64)
	if err != nil || load > o.MaxLoad*float64(runtime.NumCPU()) {
		return false
	}
	return memAvailableMB() >= o.MinFreeMB
}

func memAvailableMB() int64 {
	fh, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}

// storeQuiet: ninguém (nem outro processo, como o CLI) escreveu no depósito
// há pelo menos d. O index.log recebe toda escrita.
func (s *Store) storeQuiet(d time.Duration) bool {
	st, err := os.Stat(filepath.Join(s.StoreDir(), "index.log"))
	if err != nil {
		return false // sem depósito de blocos: nada a fazer
	}
	return time.Since(st.ModTime()) >= d
}

// CompactIdleOnce faz uma passada: se está tudo parado, recomprime rodada a
// rodada enquanto continuar parado. Devolve quantos bytes saíram e se ainda
// sobra trabalho.
func (s *Store) CompactIdleOnce(ctx context.Context, o IdleOptions) (saved int64, more bool, err error) {
	o.defaults()
	idle := o.Idle
	if idle == nil {
		idle = func() bool { return systemIdle(o) }
	}
	for {
		if ctx.Err() != nil {
			return saved, true, nil
		}
		s.mu.Lock()
		busy := s.activeBatch != nil
		s.mu.Unlock()
		if busy || !s.storeQuiet(o.Quiet) || !idle() {
			return saved, true, nil
		}
		bs, err := s.Blocks()
		if err != nil {
			return saved, false, err
		}
		r, err := bs.Recompress(o.RoundSize)
		if err != nil {
			return saved, false, err
		}
		saved += r.Before - r.After
		if r.Remaining == 0 {
			return saved, false, nil
		}
		// a rodada mexeu no index.log: o "parado" agora é medido pela carga
		o.Quiet = time.Nanosecond
	}
}

// RunIdleCompact roda o laço até ctx acabar. Quando a recompressão termina,
// faz também uma limpeza leve (GC), que pega os blocos que só saem numa
// segunda passada. Escreve no log (journal do residente).
func (s *Store) RunIdleCompact(ctx context.Context, o IdleOptions) {
	o.defaults()
	t := time.NewTicker(o.Every)
	defer t.Stop()
	var lastGC time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		saved, more, err := s.CompactIdleOnce(ctx, o)
		if err != nil {
			log.Printf("compactar em ocioso: %v", err)
			continue
		}
		if saved > 0 {
			log.Printf("compactar em ocioso: %d bytes a menos", saved)
		}
		if more || time.Since(lastGC) < 6*time.Hour || !s.storeQuiet(o.Quiet) {
			continue
		}
		lastGC = time.Now()
		if r, err := s.GCBlocks(0.3); err != nil {
			log.Printf("limpeza em ocioso: %v", err)
		} else if r.DeadChunks > 0 {
			log.Printf("limpeza em ocioso: %d blocos sem dono removidos", r.DeadChunks)
		}
	}
}
