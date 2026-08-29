// crom-ar é o gerenciador de arquivos .crom (estilo WinRAR) do CROM:
// cofre local deduplicado que cresce conforme você importa dados de qualquer tipo.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"

	"github.com/MrJc01/crom-crompressor-ar/internal/bench"
	"github.com/MrJc01/crompressor/pkg/cromlib"

	"github.com/MrJc01/crom-crompressor-ar/internal/bundle"
	"github.com/MrJc01/crom-crompressor-ar/internal/fusefs"
	"github.com/MrJc01/crom-crompressor-ar/internal/server"
	"github.com/MrJc01/crom-crompressor-ar/internal/tray"
	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

const usage = `crom-ar — cofre local deduplicado de arquivos .crom (estilo WinRAR)

Uso:
  crom-ar <comando> [opções]

Comandos:
  gui | serve                   Abre a GUI local (navegador, 127.0.0.1:8619)
  fuse                          Monta o cofre como pasta real (drag & drop nativo)
  tray                          Ícone residente na bandeija (estado + ações)
  install                       Instala: binário, ícone na área de trabalho e cofre
  init [--root DIR]             Inicializa o cofre
  add [opts] <caminho...>       Importa arquivos/pastas para o cofre (aceita qualquer dado)
  list [--json]                 Lista o conteúdo do cofre
  extract <id|nome|grupo>       Recompõe o .crom no HD local
  rm <termo...>                 Remove entradas (e .crom órfãos)
  verify <termo>                Reconfere integridade lossless de entradas
  info <termo>                  Estatísticas detalhadas do .crom (crompressor info)
  stats                         Resumo do cofre (dedup, ratio, espaço)
  train [-i dir] [--size N]     (Re)treina o codebook do cofre
  brain <sub> [args]            Codebooks: list, add, use, remove, export, train
  export <pacote> -o X.cromar   Exporta um pacote portátil auto-contido
  cd mount|import|list ...      Monta/importa/lista um .cromar (modo CD)
  extract-file <pacote.cromar>  Extrai os membros direto para o HD (Nemo)
  edge pack|unpack              Esqueleto lossy (sem deltas) via cromlib
  bench <arquivo|pasta>         Testa estratégias de compressão e recomenda a melhor
  mount <termo> -m <ponto>      Monta o .crom como filesystem (VFS, exige FUSE)

Variáveis:
  CROMPRESSOR_BIN   caminho do binário crompressor
  CROM_AR_ROOT      raiz do cofre (padrão: ~/Documentos/CromAr)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "gui", "serve":
		err = cmdServe(os.Args[2:])
	case "fuse":
		err = cmdFuse(os.Args[2:])
	case "tray":
		err = cmdTray(os.Args[2:])
	case "brain":
		err = cmdBrain(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "cd":
		err = cmdCD(os.Args[2:])
	case "extract-file":
		err = cmdExtractFile(os.Args[2:])
	case "edge":
		err = cmdEdge(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "extract":
		err = cmdExtract(os.Args[2:])
	case "rm":
		err = cmdRm(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "info":
		err = cmdInfo(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "train":
		err = cmdTrain(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "mount":
		err = cmdMount(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "comando desconhecido: %s\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
}

func rootDir() string {
	if r := os.Getenv("CROM_AR_ROOT"); r != "" {
		return vault.ExpandHome(r)
	}
	r, _ := vault.DefaultRoot()
	// compatibilidade: cofre antigo em ~/.crom-ar continua válido
	if _, err := os.Stat(filepath.Join(r, "config.json")); err != nil {
		if legacy, err := vault.LegacyRoot(); err == nil {
			if _, err2 := os.Stat(filepath.Join(legacy, "config.json")); err2 == nil {
				return legacy
			}
		}
	}
	return r
}

// parseFlex permite flags antes E depois dos argumentos posicionais
// (ex.: `crom-ar extract nome -o saida`).
func parseFlex(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	return pos, nil
}

func openStore() (*vault.Store, error) {
	s, err := vault.LoadStore(rootDir())
	if err != nil {
		return nil, err
	}
	return s, nil
}

// cmdExport gera um pacote portátil .cromar (membros + codebook).
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	out := fs.String("o", "", "saída .cromar")
	brain := fs.String("brain", "", "codebook para embutir (padrão: dos membros/cofre)")
	esqueleto := fs.Bool("esqueleto", false, "exporta esqueleto lossy (sem deltas, leitura aproximada)")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		return fmt.Errorf("uso: crom-ar export <pacote> -o pacote.cromar")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	n, err := bundle.Export(s, pos[0], *out, *brain, *esqueleto)
	if err != nil {
		return err
	}
	st, _ := os.Stat(vault.ExpandHome(*out))
	tipo := "self-contained lossless"
	if *esqueleto {
		tipo = "ESQUELETO lossy (leitura aproximada — deltas não inclusos)"
	}
	fmt.Printf("✔ pacote portátil: %s (%d membros, %s, %s)\n", *out, n, vault.HumanBytes(st.Size()), tipo)
	return nil
}

// cmdCD monta/importa/lista um .cromar (modo CD, somente-leitura).
func cmdCD(args []string) error {
	if len(args) == 0 {
		fmt.Println("uso: crom-ar cd <mount|import|list> [args]")
		return nil
	}
	switch args[0] {
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("uso: crom-ar cd list <pacote.cromar>")
		}
		meta, _, err := bundle.Open(openStoreOrDie(), args[1])
		if err != nil {
			return err
		}
		fmt.Printf("pacote: %s | codebook: %s | membros: %d\n", meta.Group, meta.Brain, len(meta.Members))
		for _, m := range meta.Members {
			fmt.Printf("  %-50s %10s\n", m.Name, vault.HumanBytes(m.Size))
		}
		return nil
	case "import":
		fs := flag.NewFlagSet("cd import", flag.ExitOnError)
		into := fs.String("into", "", "nome do grupo/pacote destino (padrão: o original)")
		pos, err := parseFlex(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return fmt.Errorf("uso: crom-ar cd import <pacote.cromar> [--into nome]")
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		n, err := bundle.Import(s, pos[0], *into)
		if err != nil {
			return err
		}
		if err := s.Save(); err != nil {
			return err
		}
		fmt.Printf("✔ %d membro(s) importado(s) do pacote\n", n)
		return nil
	case "mount":
		fs := flag.NewFlagSet("cd mount", flag.ExitOnError)
		mp := fs.String("m", "", "ponto de montagem")
		daemon := fs.Bool("daemon", false, "rodar desprendido (log em /tmp/crom-ar-cd.log)")
		fg := fs.Bool("foreground", false, "uso interno do --daemon")
		pos, err := parseFlex(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 || *mp == "" {
			return fmt.Errorf("uso: crom-ar cd mount <pacote.cromar> -m <ponto> [--daemon]")
		}
		mountpoint := vault.ExpandHome(*mp)
		if fusefs.IsMounted(mountpoint) {
			fmt.Println("já montado em", mountpoint)
			return nil
		}
		if *fg {
			return cdMountRun(pos[0], mountpoint)
		}
		if *daemon {
			logPath := filepath.Join(os.TempDir(), "crom-ar-cd.log")
			logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			defer logf.Close()
			self, _ := os.Executable()
			cmd := exec.Command(self, []string{"cd", "mount", pos[0], "-m", mountpoint, "--foreground"}...)
			cmd.Stdin = nil
			cmd.Stdout = logf
			cmd.Stderr = logf
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				return err
			}
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if fusefs.IsMounted(mountpoint) {
					fmt.Printf("✔ pacote montado (read-only) em %s (log: %s)\n", mountpoint, logPath)
					return nil
				}
				time.Sleep(150 * time.Millisecond)
			}
			return fmt.Errorf("timeout aguardando montagem (veja %s)", logPath)
		}
		// daemoniza (mesmo padrão do fuse)
		logPath := filepath.Join(os.TempDir(), "crom-ar-cd.log")
		logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		self, _ := os.Executable()
		cmd := exec.Command(self, []string{"cd", "mount", pos[0], "-m", mountpoint, "--foreground"}...)
		cmd.Stdin = nil
		cmd.Stdout = logf
		cmd.Stderr = logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if fusefs.IsMounted(mountpoint) {
				fmt.Printf("✔ pacote montado (read-only) em %s\n", mountpoint)
				return nil
			}
			time.Sleep(150 * time.Millisecond)
		}
		return fmt.Errorf("timeout aguardando montagem (veja %s)", logPath)
	default:
		return fmt.Errorf("subcomando desconhecido: %s (mount, import, list)", args[0])
	}
}

func cdMountRun(bundlePath, mountpoint string) error {
	s, err := openStore()
	if err != nil {
		return err
	}
	meta, srcDir, err := bundle.Open(s, bundlePath)
	if err != nil {
		return err
	}
	srv, err := fusefs.MountLoopbackRO(srcDir, mountpoint)
	if err != nil {
		return err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		return fmt.Errorf("mount não respondeu: %v", err)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	aviso := ""
	if meta.Mode == "edge" {
		aviso = " ⚠ MODO ESQUELETO: conteúdo APROXIMADO (sem deltas)"
	}
	fmt.Printf("📦 pacote %q montado (read-only) em %s — Ctrl+C para desmontar%s\n", meta.Group, mountpoint, aviso)
	<-sigCh
	srv.Unmount()
	return nil
}

func openStoreOrDie() *vault.Store {
	s, err := openStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
	return s
}

// cmdExtractFile extrai membros de um .cromar direto no HD (action do Nemo).
func cmdExtractFile(args []string) error {
	fs := flag.NewFlagSet("extract-file", flag.ExitOnError)
	out := fs.String("o", "", "diretório de destino (padrão: pasta do arquivo)")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("uso: crom-ar extract-file <pacote.cromar> [-o dir]")
	}
	dest := *out
	if dest == "" {
		dest = filepath.Dir(vault.ExpandHome(pos[0]))
	}
	n, err := bundle.ExtractFile(pos[0], dest)
	if err != nil {
		return err
	}
	fmt.Printf("✔ %d arquivo(s) extraído(s) em %s\n", n, dest)
	return nil
}

// cmdEdge empacota/desempacota no modo esqueleto (lossy, sem deltas).
// Ler sem o delta que monta: a reconstrução usa o codebook puro.
func cmdEdge(args []string) error {
	if len(args) == 0 {
		fmt.Println("uso: crom-ar edge <pack|unpack> -i entrada [-o saída] --brain codebook.cromdb")
		return nil
	}
	fs := flag.NewFlagSet("edge", flag.ExitOnError)
	out := fs.String("o", "", "saída")
	brain := fs.String("brain", "", "codebook .cromdb (obrigatório)")
	pos, err := parseFlex(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" || *brain == "" {
		return fmt.Errorf("uso: crom-ar edge pack -i entrada -o saida.crom --brain codebook.cromdb")
	}
	brainPath := vault.ExpandHome(*brain)
	switch args[0] {
	case "pack":
		opts := cromlib.DefaultPackOptions()
		opts.Mode = "edge"
		opts.UseCDC = true
		m, err := cromlib.Pack(vault.ExpandHome(pos[0]), vault.ExpandHome(*out), brainPath, opts)
		if err != nil {
			return err
		}
		fmt.Printf("✔ esqueleto gerado: %s → %s (hit rate %.1f%%, lossy: fidelidade ≈ hit rate)\n",
			pos[0], *out, m.HitRate)
		return nil
	case "unpack":
		uopts := cromlib.DefaultUnpackOptions()
		uopts.Fuzziness = 0.0001 // edge: hash-check desligado (conteúdo aproximado)
		if err := cromlib.Unpack(vault.ExpandHome(pos[0]), vault.ExpandHome(*out), brainPath, uopts); err != nil {
			return err
		}
		fmt.Printf("✔ reconstruído (lossy, sem deltas): %s\n", *out)
		return nil
	default:
		return fmt.Errorf("subcomando desconhecido: %s (pack, unpack)", args[0])
	}
}

// cmdBrain gerencia codebooks nomeados (compartilháveis entre cofres).
func cmdBrain(args []string) error {
	if len(args) == 0 {
		fmt.Print(`uso: crom-ar brain <subcomando> [args]

  list                        Lista os codebooks do cofre
  add <arquivo.cromdb> [nome] Importa um codebook (de terceiros ou backup)
  use <nome>                  Define o codebook padrão dos próximos imports
  remove <nome>               Remove um codebook (não o padrão)
  export <nome> -o saída      Copia um codebook para fora (compartilhar)
  train <nome> -i <dir>       Treina um codebook nomeado a partir de um diretório`)
		return nil
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		brains := s.ListBrains()
		if len(brains) == 0 {
			fmt.Println("nenhum codebook — o primeiro import semeia automaticamente")
			return nil
		}
		for _, b := range brains {
			def := ""
			if b.Default {
				def = "  ← padrão"
			}
			fmt.Printf("%-20s %8s %s%s\n", b.Name, vault.HumanBytes(b.Size), b.Path, def)
		}
		return nil
	case "add", "import":
		fs := flag.NewFlagSet("brain add", flag.ExitOnError)
		name := fs.String("n", "", "nome do codebook")
		pos, err := parseFlex(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return fmt.Errorf("uso: crom-ar brain add <arquivo.cromdb> [nome]")
		}
		nome := *name
		if nome == "" && len(pos) > 1 {
			nome = pos[1]
		}
		n, err := s.AddBrain(pos[0], nome)
		if err != nil {
			return err
		}
		fmt.Printf("✔ codebook importado: %s\n", n)
		return s.Save()
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("uso: crom-ar brain use <nome>")
		}
		if err := s.UseBrain(args[1]); err != nil {
			return err
		}
		fmt.Printf("✔ codebook padrão: %s\n", s.Cfg.DefaultBrain)
		return nil
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("uso: crom-ar brain remove <nome>")
		}
		if err := s.RemoveBrain(args[1]); err != nil {
			return err
		}
		fmt.Printf("✔ removido: %s\n", args[1])
		return nil
	case "export":
		fs := flag.NewFlagSet("brain export", flag.ExitOnError)
		out := fs.String("o", "", "arquivo de saída")
		pos, err := parseFlex(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) == 0 || *out == "" {
			return fmt.Errorf("uso: crom-ar brain export <nome> -o saída.cromdb")
		}
		if err := s.ExportBrain(pos[0], *out); err != nil {
			return err
		}
		fmt.Printf("✔ codebook exportado: %s\n", *out)
		return nil
	case "train":
		fs := flag.NewFlagSet("brain train", flag.ExitOnError)
		in := fs.String("i", "", "diretório com dados representativos")
		size := fs.Int("size", 8192, "número de padrões")
		pos, err := parseFlex(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) == 0 || *in == "" {
			return fmt.Errorf("uso: crom-ar brain train <nome> -i <diretório>")
		}
		out, err := s.TrainNamedBrain(pos[0], *in, *size)
		if err != nil {
			return err
		}
		fmt.Printf("✔ codebook treinado: %s\n", out)
		return nil
	default:
		return fmt.Errorf("subcomando desconhecido: %s (list, add, use, remove, export, train)", args[0])
	}
}

// cmdTray roda o ícone residente na bandeija (estado, montar, painel web).
func cmdTray(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ExitOnError)
	mp := fs.String("m", "", "ponto de montagem (padrão: ~/CromAr)")
	_ = fs.String("root", rootDir(), "raiz do cofre")
	noWeb := fs.Bool("no-web", false, "inicia com o painel web desligado")
	noAuto := fs.Bool("no-automount", false, "não monta o cofre ao iniciar")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}

	// single-instance via pidfile
	pidPath := filepath.Join(vault.ExpandHome("~/.local/share/crom-ar"), "tray.pid")
	if b, err := os.ReadFile(pidPath); err == nil {
		if alive := signalCheck(strings.TrimSpace(string(b))); alive {
			fmt.Println("crom-ar tray já está rodando")
			return nil
		}
	}
	_ = os.MkdirAll(filepath.Dir(pidPath), 0o700)
	_ = os.WriteFile(pidPath, []byte(fmt.Sprint(os.Getpid())), 0o600)
	defer os.Remove(pidPath)

	mountpoint := *mp
	if mountpoint == "" {
		mountpoint = filepath.Join(homeDir(), "CromAr")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	if _, err := server.EnsureToken(); err != nil {
		return err
	}
	return tray.Run(s, tray.Options{
		Mountpoint:  mountpoint,
		Root:        s.Cfg.Root,
		NoWeb:       *noWeb,
		NoAutoMount: *noAuto,
	})
}

func signalCheck(pidStr string) bool {
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "crom-ar")
}

// cmdFuse monta o cofre como filesystem real (drag & drop nativo).
func cmdFuse(args []string) error {
	fs := flag.NewFlagSet("fuse", flag.ExitOnError)
	mp := fs.String("m", "", "ponto de montagem (padrão: ~/CromAr)")
	root := fs.String("root", rootDir(), "raiz do cofre")
	brain := fs.String("brain", "", "codebook para escrita (padrão: do cofre)")
	noVerify := fs.Bool("no-verify", false, "pular verificação lossless na escrita")
	unmount := fs.Bool("u", false, "desmontar o mountpoint e sair")
	daemon := fs.Bool("daemon", false, "rodar desprendido do terminal (log em <root>/fuse.log)")
	foreground := fs.Bool("foreground", false, "uso interno do --daemon")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}
	_ = foreground

	mountpoint := *mp
	if mountpoint == "" {
		mountpoint = filepath.Join(homeDir(), "CromAr")
	}
	mountpoint = vault.ExpandHome(mountpoint)

	if *unmount {
		if exec.Command("fusermount3", "-u", mountpoint).Run() != nil {
			return exec.Command("fusermount", "-u", mountpoint).Run()
		}
		fmt.Println("✔ desmontado:", mountpoint)
		return nil
	}

	if fusefs.IsMounted(mountpoint) {
		fmt.Println("já montado em", mountpoint)
		return nil
	}

	// daemoniza: re-executa em nova sessão com stdio no log, pai sai na hora
	if *daemon {
		logPath := filepath.Join(vault.ExpandHome(*root), "fuse.log")
		logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		self, _ := os.Executable()
		childArgs := []string{"fuse", "-m", mountpoint, "--root", *root, "--foreground"}
		if *brain != "" {
			childArgs = append(childArgs, "--brain", *brain)
		}
		if *noVerify {
			childArgs = append(childArgs, "--no-verify")
		}
		cmd := exec.Command(self, childArgs...)
		cmd.Stdin = nil
		cmd.Stdout = logf
		cmd.Stderr = logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		// aguarda o mount aparecer (ou o filho morrer)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if fusefs.IsMounted(mountpoint) {
				fmt.Printf("✔ cofre nativo montado em %s (log: %s)\n", mountpoint, logPath)
				return nil
			}
			if cmd.ProcessState != nil {
				return fmt.Errorf("daemon morreu na inicialização (veja %s)", logPath)
			}
			time.Sleep(150 * time.Millisecond)
		}
		return fmt.Errorf("timeout aguardando montagem (veja %s)", logPath)
	}
	return fuseRun(mountpoint, *root, *brain, *noVerify)
}

func fuseRun(mountpoint, root, brain string, noVerify bool) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return err
	}
	s, err := vault.LoadStore(root)
	if err != nil {
		return err
	}
	vfs := fusefs.New(s, fusefs.Options{Brain: brain, NoVerify: noVerify})
	nodeFs := pathfs.NewPathNodeFs(vfs, nil)
	conn := nodefs.NewFileSystemConnector(nodeFs.Root(), &nodefs.Options{})
	srv, err := fuse.NewServer(conn.RawFS(), mountpoint, fusefs.MountOptions())
	if err != nil {
		return fmt.Errorf("montagem falhou (fuse3 instalado?): %v", err)
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		return fmt.Errorf("mount não respondeu: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	fmt.Printf("🧬 cofre nativo em %s  (cofre: %s — Ctrl+C para desmontar)\n", mountpoint, s.Cfg.Root)
	<-sigCh
	fmt.Println("\ndesmontando…")
	srv.Unmount()
	return nil
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// cmdServe abre a GUI local no navegador (estilo WinRAR).
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8619", "endereço de escuta (somente localhost)")
	open := fs.Bool("open", true, "abrir o navegador automaticamente")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	tok, err := server.EnsureToken()
	if err != nil {
		return fmt.Errorf("token: %v", err)
	}
	sv := server.New(s, *addr)
	sv.Token = tok
	url := "http://" + *addr + "/?t=" + tok
	if server.PortBusy(*addr) {
		fmt.Println("GUI já rodando em", url)
		if *open {
			exec.Command("xdg-open", url).Start()
		}
		return nil
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		if *open {
			exec.Command("xdg-open", url).Start()
		}
	}()
	fmt.Printf("🧬 CROM-AR GUI em %s  (cofre: %s)\n", "http://"+*addr, s.Cfg.Root)
	return sv.ListenAndServe()
}

// cmdInstall instala binário, ícone e lançador na máquina.
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	autostart := fs.Bool("autostart", false, "ativa o crom-ar residente no login (systemd --user)")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if dst := filepath.Join(binDir, "crom-ar"); exe != dst {
		if err := copyFile(exe, dst, 0o755); err != nil {
			return err
		}
	}

	// crompressor junto (a GUI precisa dele)
	cromBin, err := vault.FindBinary()
	if err != nil {
		return fmt.Errorf("crompressor não encontrado: %v (defina CROMPRESSOR_BIN)", err)
	}
	if err := copyFile(cromBin, filepath.Join(binDir, "crompressor"), 0o755); err != nil {
		return err
	}

	// ícone SVG
	iconDir := filepath.Join(home, ".local", "share", "icons", "hicolor", "scalable", "apps")
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		return err
	}
	iconPath := filepath.Join(iconDir, "crom-ar.svg")
	if err := os.WriteFile(iconPath, []byte(iconSVG), 0o644); err != nil {
		return err
	}

	// lançador .desktop
	appsDir := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return err
	}
	// script de abertura: monta o cofre nativo e abre no gerenciador de arquivos
	openScript := fmt.Sprintf(`#!/bin/sh
# CROM-AR: monta o cofre nativo (se ainda não estiver) e abre no gerenciador
MP="$HOME/CromAr"
"%s" fuse --daemon || exit 1
if command -v nemo >/dev/null 2>&1; then
    nemo "$MP" >/dev/null 2>&1 &
elif command -v nautilus >/dev/null 2>&1; then
    nautilus "$MP" >/dev/null 2>&1 &
else
    xdg-open "$MP" >/dev/null 2>&1 &
fi
`, filepath.Join(binDir, "crom-ar"))
	openPath := filepath.Join(binDir, "crom-ar-open")
	if err := os.WriteFile(openPath, []byte(openScript), 0o755); err != nil {
		return err
	}

	desktop := fmt.Sprintf(desktopTemplate,
		filepath.Join(binDir, "crom-ar"),
		filepath.Join(binDir, "crom-ar"),
		filepath.Join(binDir, "crom-ar"),
		filepath.Join(binDir, "crom-ar"))
	desktopPath := filepath.Join(appsDir, "crom-ar.desktop")
	if err := os.WriteFile(desktopPath, []byte(desktop), 0o644); err != nil {
		return err
	}

	// atalho na área de trabalho, se existir
	desk := desktopDir(home)
	if desk != "" {
		userDesk := filepath.Join(desk, "crom-ar.desktop")
		if err := os.WriteFile(userDesk, []byte(desktop), 0o755); err == nil {
			fmt.Println("✔ atalho criado na área de trabalho:", userDesk)
		}
	}

	// action do Nemo: extrair pacote .cromar com botão direito
	nemoDir := filepath.Join(home, ".local", "share", "nemo", "actions")
	if err := os.MkdirAll(nemoDir, 0o755); err != nil {
		return err
	}
	nemo := fmt.Sprintf(nemoTemplate, filepath.Join(binDir, "crom-ar"))
	if err := os.WriteFile(filepath.Join(nemoDir, "crom-ar-extract.nemo_action"), []byte(nemo), 0o644); err != nil {
		return err
	}

	// mimetype application/x-cromar
	mimeDir := filepath.Join(home, ".local", "share", "mime", "packages")
	if err := os.MkdirAll(mimeDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mimeDir, "x-cromar.xml"), []byte(mimeXML), 0o644); err != nil {
		return err
	}
	_ = exec.Command("update-mime-database", filepath.Join(home, ".local", "share", "mime")).Run()

	// residente no login (systemd --user, opt-in)
	if *autostart {
		unitDir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(unitDir, 0o755); err != nil {
			return err
		}
		unit := fmt.Sprintf(systemdTemplate, filepath.Join(binDir, "crom-ar"))
		unitPath := filepath.Join(unitDir, "crom-ar.service")
		if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		if err := exec.Command("systemctl", "--user", "enable", "--now", "crom-ar.service").Run(); err != nil {
			fmt.Println("! não consegui ativar o serviço (systemd user indisponível):", err)
		} else {
			fmt.Println("✔ crom-ar residente ativado no login (systemctl --user)")
		}
	}

	// garante cofre padrão e token da GUI
	s, err := vault.LoadStore(rootDir())
	if err != nil {
		return err
	}
	if err := s.Save(); err != nil {
		return err
	}
	if _, err := server.EnsureToken(); err != nil {
		return err
	}

	fmt.Println("✔ crom-ar instalado em", filepath.Join(binDir, "crom-ar"))
	fmt.Println("✔ crompressor em", filepath.Join(binDir, "crompressor"))
	fmt.Println("✔ lançador:", desktopPath)
	fmt.Println("✔ cofre:", s.Cfg.Root)
	fmt.Println("Abra pelo ícone da área de trabalho ou rode: crom-ar gui")
	return nil
}

func desktopDir(home string) string {
	if out, err := exec.Command("xdg-user-dir", "DESKTOP").Output(); err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" {
			return p
		}
	}
	for _, c := range []string{"Área de Trabalho", "Desktop"} {
		p := filepath.Join(home, c)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
	}
	return ""
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

const desktopTemplate = `[Desktop Entry]
Type=Application
Version=1.0
Name=CROM-AR
GenericName=Cofre .crom
Comment=Cofre local deduplicado de arquivos .crom (CROM)
Exec=%s tray
Icon=crom-ar
Terminal=false
Categories=Utility;Archiving;FileTools;TrayIcon;
Keywords=crom;compress;backup;cofre;
StartupWMClass=crom-ar
Actions=open;web;unmount;

[Desktop Action open]
Name=Abrir cofre no gerenciador
Exec=%s-open

[Desktop Action web]
Name=Painel crom-ar (GUI)
Exec=%s gui

[Desktop Action unmount]
Name=Desmontar cofre
Exec=%s fuse -u
`

const nemoTemplate = `[Nemo Action]
Name=Extrair com crom-ar
Comment=Recompõe os arquivos do pacote .cromar no HD local
Exec=%s extract-file %%F
Icon-Name=package-x-generic
Selection=s
Extensions=cromar;
EscapeSpaces=true
`

const mimeXML = `<?xml version="1.0" encoding="UTF-8"?>
<mime-info xmlns="http://www.freedesktop.org/standards/shared-mime-info">
  <mime-type type="application/x-cromar">
    <comment>Pacote portátil CROM-AR</comment>
    <icon name="crom-ar"/>
    <glob pattern="*.cromar"/>
  </mime-type>
  <mime-type type="application/x-crom">
    <comment>Arquivo comprimido CROM</comment>
    <icon name="crom-ar"/>
    <glob pattern="*.crom"/>
  </mime-type>
</mime-info>
`

const systemdTemplate = `[Unit]
Description=CROM-AR residente (tray + cofre nativo + painel web)
After=graphical-session.target

[Service]
ExecStart=%s tray
Restart=on-failure

[Install]
WantedBy=default.target
`

const iconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128">
<defs>
<linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
<stop offset="0" stop-color="#134e4a"/><stop offset=".55" stop-color="#155e75"/><stop offset="1" stop-color="#1e3a8a"/>
</linearGradient>
</defs>
<rect x="6" y="6" width="116" height="116" rx="26" fill="url(#g)"/>
<path d="M64 22 L98 41 V87 L64 106 L30 87 V41 Z" fill="none" stroke="#5eead4" stroke-width="5" stroke-linejoin="round"/>
<path d="M30 41 L64 60 L98 41 M64 60 V106" stroke="#5eead4" stroke-width="3" opacity=".65" fill="none"/>
<path d="M78 52 a20 20 0 1 0 0 24" fill="none" stroke="#818cf8" stroke-width="9" stroke-linecap="round"/>
</svg>
`

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	root := fs.String("root", rootDir(), "diretório raiz do cofre")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}
	s, err := vault.LoadStore(*root)
	if err != nil {
		return err
	}
	if err := s.Save(); err != nil {
		return err
	}
	fmt.Printf("✔ cofre inicializado em %s\n", s.Cfg.Root)
	fmt.Printf("  vault:   %s (.crom)\n  brains:  %s (codebooks)\n  binário: %s\n",
		s.Cfg.VaultDir, s.Cfg.BrainsDir, s.Cfg.CrompressorBin)
	return nil
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	cdc := fs.Bool("cdc", true, "Content-Defined Chunking (recomendado)")
	multi := fs.Bool("multi-pass", false, "compressão em duas passagens")
	enc := fs.String("encrypt", "", "senha AES-256-GCM")
	brain := fs.String("brain", "", "codebook .cromdb específico (padrão: brains/vault.cromdb)")
	forceTrain := fs.Bool("train", false, "retreinar codebook do cofre a partir da origem")
	noVerify := fs.Bool("no-verify", false, "pular verificação lossless pós-pack")
	noBrain := fs.Bool("no-brain", false, "expert: empacotar sem codebook (não-confiável no V24)")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("informe o caminho para importar")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	opt := vault.AddOptions{CDC: *cdc, MultiPass: *multi, Encrypt: *enc, Brain: *brain, NoVerify: *noVerify, ForceTrain: *forceTrain, NoBrain: *noBrain}
	for _, p := range pos {
		res, err := s.Add(p, opt)
		if err != nil {
			return err
		}
		for _, r := range res {
			tag := ""
			if r.Dedup {
				tag = " [dedup]"
			}
			if !r.Entry.Verified {
				tag += " [não verificado]"
			}
			fmt.Printf("✔ %-40s %10s → %10s (%5.1f%%)%s\n",
				r.Entry.RelPath, vault.HumanBytes(r.Entry.Size), vault.HumanBytes(r.Entry.CromSize),
				ratio(r.Entry.Size, r.Entry.CromSize), tag)
			if r.Msg != "" {
				fmt.Printf("   ↳ %s\n", r.Msg)
			}
		}
	}
	return s.Save()
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "saída em JSON")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(s.Man)
	}
	if len(s.Man.Entries) == 0 {
		fmt.Println("cofre vazio — use: crom-ar add <caminho>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID (sha256)\tNOME\tTAM\tCROM\tRATIO\tGRUPO\tQUANDO")
	for _, e := range s.Man.Entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.1f%%\t%s\t%s\n",
			e.ID[:12], e.RelPath, vault.HumanBytes(e.Size), vault.HumanBytes(e.CromSize),
			ratio(e.Size, e.CromSize), e.Group, e.PackedAt.Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	out := fs.String("o", "crom-restore", "diretório de destino no HD local")
	force := fs.Bool("force", false, "sobrescrever arquivos existentes")
	enc := fs.String("encrypt", "", "senha de descriptografia")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("uso: crom-ar extract <id|nome|grupo>")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	entries := s.Match(pos[0])
	if len(entries) == 0 {
		return fmt.Errorf("nada encontrado para %q", pos[0])
	}
	fmt.Printf("extraindo %d arquivo(s) para %s ...\n", len(entries), *out)
	results, err := s.Extract(entries, vault.ExtractOptions{OutDir: *out, Force: *force, EncKey: *enc})
	if err != nil {
		return err
	}
	n, fail := 0, 0
	for _, r := range results {
		if r.Ok {
			fmt.Printf("✔ %s\n", r.Target)
			n++
		} else {
			fmt.Fprintf(os.Stderr, "✘ %s: %s\n", r.Name, r.Err)
			fail++
		}
	}
	if fail > 0 {
		return fmt.Errorf("%d arquivo(s) falharam", fail)
	}
	fmt.Printf("✔ %d arquivo(s) recompilado(s) com integridade SHA-256\n", n)
	return nil
}

func cmdRm(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("uso: crom-ar rm <termo...>")
	}
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	var all []*vault.Entry
	for _, t := range pos {
		all = append(all, s.Match(t)...)
	}
	if len(all) == 0 {
		return fmt.Errorf("nada encontrado")
	}
	n, err := s.Remove(all)
	if err != nil {
		return err
	}
	fmt.Printf("✔ %d entrada(s) removida(s)\n", n)
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	enc := fs.String("encrypt", "", "senha se a entrada for criptografada")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("uso: crom-ar verify <termo>")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	entries := s.Match(pos[0])
	if len(entries) == 0 {
		return fmt.Errorf("nada encontrado para %q", pos[0])
	}
	ok, fail := 0, 0
	for _, e := range entries {
		if err := s.VerifyEntry(e, *enc); err != nil {
			fmt.Printf("✘ %s: %v\n", e.RelPath, err)
			fail++
			continue
		}
		fmt.Printf("✔ %s (sha256 %s)\n", e.RelPath, e.ID[:12])
		ok++
	}
	if fail > 0 {
		return fmt.Errorf("%d arquivo(s) com falha", fail)
	}
	return nil
}

func cmdInfo(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("uso: crom-ar info <termo>")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	entries := s.Match(args[0])
	if len(entries) == 0 {
		return fmt.Errorf("nada encontrado para %q", args[0])
	}
	e := entries[0]
	args2 := []string{"info", "-i", s.CromPath(e)}
	if e.Codebook != "" {
		args2 = append(args2, "-c", e.Codebook)
	}
	return s.RunPassthrough(args2...)
}

func cmdStats(args []string) error {
	s, err := openStore()
	if err != nil {
		return err
	}
	st := s.ComputeStats()
	fmt.Println("═══ CROM-AR VAULT ═══")
	fmt.Printf("  arquivos lógicos:   %d\n", st.Files)
	fmt.Printf("  conteúdos únicos:   %d\n", st.Unique)
	fmt.Printf("  origem total:       %s\n", vault.HumanBytes(st.Original))
	fmt.Printf("  únicos:             %s\n", vault.HumanBytes(st.UniqueBytes))
	fmt.Printf("  armazenado (.crom): %s\n", vault.HumanBytes(st.Stored))
	fmt.Printf("  economia dedup:     %s\n", vault.HumanBytes(st.DedupSaved))
	fmt.Printf("  ratio médio:        %.1f%%\n", st.AvgRatio)
	fmt.Printf("  disco (vault/):     %s\n", vault.HumanBytes(st.VaultOnDisk))
	return nil
}

func cmdTrain(args []string) error {
	fs := flag.NewFlagSet("train", flag.ExitOnError)
	in := fs.String("i", "", "diretório com dados representativos (padrão: origem das entradas)")
	size := fs.Int("size", 8192, "número de padrões no codebook")
	if _, err := parseFlex(fs, args); err != nil {
		return err
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	dir := *in
	if dir == "" {
		if len(s.Man.Entries) == 0 {
			return fmt.Errorf("cofre vazio: informe -i <diretório> para treinar")
		}
		dirs := map[string]bool{}
		for _, e := range s.Man.Entries {
			dirs[filepath.Dir(e.SourcePath)] = true
			if len(dirs) > 8 {
				break
			}
		}
		for d := range dirs {
			if sub, err := s.TrainBrainUpdate(d, *size); err == nil {
				fmt.Printf("✔ codebook atualizado com %s: %s\n", d, sub)
			}
		}
		return nil
	}
	cb, err := s.TrainBrain(dir, *size)
	if err != nil {
		return err
	}
	fmt.Printf("✔ codebook do cofre gerado: %s\n", cb)
	return nil
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	maxFiles := fs.Int("max-files", 10, "máximo de arquivos quando for pasta")
	brain := fs.String("brain", "", "codebook para comparar (padrão: do cofre / auto-treino)")
	enc := fs.String("encrypt", "", "senha (se usar criptografia)")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("uso: crom-ar bench <arquivo|pasta>")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	rows, _, err := bench.Run(s, pos[0], bench.Options{MaxFiles: *maxFiles, Brain: *brain, Encrypt: *enc})
	if err != nil {
		return err
	}
	bench.Print(rows)
	return nil
}

func cmdMount(args []string) error {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	mp := fs.String("m", "", "ponto de montagem (diretório vazio)")
	cache := fs.Int("cache", 256, "limite de cache RAM em MB")
	pos, err := parseFlex(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *mp == "" {
		return fmt.Errorf("uso: crom-ar mount <termo> -m <ponto_de_montagem>")
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	entries := s.Match(pos[0])
	if len(entries) == 0 {
		return fmt.Errorf("nada encontrado para %q", pos[0])
	}
	e := entries[0]
	cb := e.Codebook
	if cb == "" {
		cb = s.DefaultBrainPath()
	}
	args2 := []string{"mount", "-i", s.CromPath(e), "-m", *mp, "--cache", fmt.Sprint(*cache)}
	if cb != "" {
		args2 = append(args2, "-c", cb)
	}
	return s.RunPassthrough(args2...)
}

func ratio(orig, crom int64) float64 {
	if orig == 0 {
		return 0
	}
	return float64(crom) / float64(orig) * 100
}
