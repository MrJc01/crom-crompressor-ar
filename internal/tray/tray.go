// Package tray implementa o ícone residente na bandeija do sistema
// (StatusNotifierItem via D-Bus): estado do cofre, montar/desmontar,
// painel web ligável e ações rápidas.
package tray

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/MrJc01/crom-crompressor-ar/internal/fusefs"
	"github.com/MrJc01/crom-crompressor-ar/internal/server"
	"github.com/MrJc01/crom-crompressor-ar/internal/vault"
)

// Options do app de bandeija.
type Options struct {
	Mountpoint  string // onde montar (ex.: ~/CromAr)
	Root        string // raiz do cofre
	WebAddr     string // endereço do painel web ("" = 127.0.0.1:8619)
	NoWeb       bool   // inicia com painel desligado
	NoAutoMount bool   // não monta automaticamente ao iniciar
}

// App é o estado residente do crom-ar na bandeija.
type App struct {
	st  *vault.Store
	opt Options

	mu      sync.Mutex
	webOn   bool
	webSrv  *server.Server
	quitting bool

	mStatus *systray.MenuItem
	mMount  *systray.MenuItem
	mWebTgl *systray.MenuItem
}

// Run executa o app de bandeija (bloqueia até Sair).
// SIGTERM (systemd stop) desmonta com elegância antes de encerrar.
func Run(st *vault.Store, opt Options) error {
	app := &App{st: st, opt: opt}
	if opt.WebAddr == "" {
		opt.WebAddr = "127.0.0.1:8619"
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		<-sigCh
		fusefs.RecoverStaleMount(opt.Mountpoint)
		if fusefs.IsMounted(opt.Mountpoint) {
			_ = exec.Command("fusermount3", "-u", opt.Mountpoint).Run()
		}
		systray.Quit()
	}()
	systray.Run(app.onReady, app.onExit)
	return nil
}

func (a *App) onReady() {
	systray.SetIcon(iconPNG())
	systray.SetTitle("CROM-AR")
	systray.SetTooltip("CROM-AR — cofre .crom")

	onClick := func(m *systray.MenuItem, fn func()) {
		go func() {
			for range m.ClickedCh {
				fn()
			}
		}()
	}

	a.mStatus = systray.AddMenuItem("", "")
	a.mStatus.Disable()
	a.mMount = systray.AddMenuItem("Montar cofre", "Monta o cofre como pasta nativa")
	onClick(a.mMount, a.toggleMount)
	onClick(systray.AddMenuItem("Abrir cofre no gerenciador", "Abre o cofre no Nemo/gerenciador"), a.openVault)
	systray.AddSeparator()
	onClick(systray.AddMenuItem("Abrir painel (navegador)", "Abre a GUI web com token"), a.openPanel)
	a.mWebTgl = systray.AddMenuItem("", "Liga/desliga o painel web")
	onClick(a.mWebTgl, a.toggleWeb)
	onClick(systray.AddMenuItem("Treinar codebook", "Retreina o codebook com os dados do cofre"), a.trainBrain)
	systray.AddSeparator()
	onClick(systray.AddMenuItem("Sair", "Encerra o crom-ar residente (mantém montagem)"), systray.Quit)

	if !a.opt.NoAutoMount && !fusefs.IsMountedHealthy(a.opt.Mountpoint) {
		a.mount()
	}
	if !a.opt.NoWeb {
		a.mu.Lock()
		a.webOn = true
		a.mu.Unlock()
		go a.serveWeb()
	}
	a.refreshMenu()
}

func (a *App) onExit() {
	a.stopWeb()
}

func (a *App) refreshMenu() {
	mounted := fusefs.IsMounted(a.opt.Mountpoint)
	a.mu.Lock()
	webOn := a.webOn
	a.mu.Unlock()
	if mounted {
		a.mStatus.SetTitle("● cofre montado em " + a.opt.Mountpoint)
		a.mMount.SetTitle("Desmontar cofre")
		systray.SetTooltip("CROM-AR — cofre montado em " + a.opt.Mountpoint)
	} else {
		a.mStatus.SetTitle("○ cofre desmontado")
		a.mMount.SetTitle("Montar cofre")
		systray.SetTooltip("CROM-AR — cofre desmontado")
	}
	if webOn {
		a.mWebTgl.SetTitle("Painel web: LIGADO (desligar)")
	} else {
		a.mWebTgl.SetTitle("Painel web: DESLIGADO (ligar)")
	}
}

func (a *App) toggleMount() {
	if fusefs.IsMounted(a.opt.Mountpoint) && !fusefs.IsMountedHealthy(a.opt.Mountpoint) {
		fusefs.RecoverStaleMount(a.opt.Mountpoint)
		a.refreshMenu()
		return
	}
	if fusefs.IsMounted(a.opt.Mountpoint) {
		if err := execFusermount(a.opt.Mountpoint); err != nil {
			fmt.Fprintf(os.Stderr, "! desmontagem falhou: %v\n", err)
		}
	} else {
		a.mount()
	}
	a.refreshMenu()
}

func (a *App) mount() {
	fusefs.RecoverStaleMount(a.opt.Mountpoint)
	if fusefs.IsMountedHealthy(a.opt.Mountpoint) {
		return
	}
	srv, _, err := fusefs.MountServer(a.opt.Mountpoint, a.st.Cfg.Root, "", false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "! montagem falhou: %v\n", err)
		return
	}
	go srv.Serve()
}

func (a *App) openVault() {
	fusefs.RecoverStaleMount(a.opt.Mountpoint)
	if !fusefs.IsMountedHealthy(a.opt.Mountpoint) {
		a.mount()
		// espera a montagem ficar saudável (até 5s)
		for i := 0; i < 25; i++ {
			if fusefs.IsMountedHealthy(a.opt.Mountpoint) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	openGUI(a.opt.Mountpoint)
}

func (a *App) openPanel() {
	tok, err := server.EnsureToken()
	if err != nil {
		return
	}
	openGUI("http://" + a.opt.WebAddr + "/?t=" + tok)
}

func (a *App) toggleWeb() {
	a.mu.Lock()
	want := !a.webOn
	a.webOn = want
	a.mu.Unlock()
	if want {
		go a.serveWeb()
	} else {
		a.stopWeb()
	}
	a.refreshMenu()
}

func (a *App) serveWeb() {
	tok, err := server.EnsureToken()
	if err != nil {
		return
	}
	a.mu.Lock()
	if a.webSrv != nil {
		a.mu.Unlock()
		return
	}
	sv := server.New(a.st, a.opt.WebAddr)
	sv.Token = tok
	a.webSrv = sv
	a.mu.Unlock()
	a.refreshMenu()
	// retorna quando Stop() for chamado pelo toggle/Sair
	if err := sv.ListenAndServe(); err != nil {
		a.mu.Lock()
		a.webOn = false
		a.mu.Unlock()
	}
	a.refreshMenu()
}

func (a *App) stopWeb() {
	a.mu.Lock()
	sv := a.webSrv
	a.webOn = false
	a.webSrv = nil
	a.mu.Unlock()
	if sv != nil {
		sv.Stop()
	}
	a.refreshMenu()
}

func (a *App) trainBrain() {
	dirs := map[string]bool{}
	for _, e := range a.st.Man.Entries {
		d := filepath.Dir(e.SourcePath)
		if len(dirs) < 8 {
			dirs[d] = true
		}
	}
	for d := range dirs {
		if _, err := a.st.TrainBrainUpdate(d, 8192); err == nil {
			return
		}
	}
	_, _ = a.st.SeedBrainFromSystem()
}

func execFusermount(mountpoint string) error {
	if err := exec.Command("fusermount3", "-u", mountpoint).Run(); err != nil {
		return exec.Command("fusermount", "-u", mountpoint).Run()
	}
	return nil
}

func openGUI(target string) {
	_ = exec.Command("xdg-open", target).Start()
}

// iconPNG gera o ícone 64x64 (cubo CROM em quadrado arredondado) sem
// depender de ferramentas externas.
func iconPNG() []byte {
	const S = 64
	img := image.NewNRGBA(image.Rect(0, 0, S, S))
	top := color.RGBA{19, 78, 74, 255}
	bot := color.RGBA{30, 58, 138, 255}
	inside := func(x, y int) bool { // retângulo arredondado
		const r = 12
		cx, cy := clampI(x, r, S-1-r), clampI(y, r, S-1-r)
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
	for y := 0; y < S; y++ {
		t := float64(y) / (S - 1)
		for x := 0; x < S; x++ {
			if inside(x, y) {
				img.Set(x, y, lerp(top, bot, t))
			}
		}
	}
	teal := color.RGBA{94, 234, 212, 255}
	line(img, 32, 12, 55, 25, teal)
	line(img, 55, 25, 55, 43, teal)
	line(img, 55, 43, 32, 56, teal)
	line(img, 32, 56, 9, 43, teal)
	line(img, 9, 43, 9, 25, teal)
	line(img, 9, 25, 32, 12, teal)
	line(img, 32, 12, 32, 32, teal)
	line(img, 32, 32, 9, 25, teal)
	line(img, 32, 32, 55, 25, teal)
	line(img, 32, 32, 32, 56, teal)
	indigo := color.RGBA{129, 140, 248, 255}
	arc(img, 36, 30, 11, math.Pi*0.35, math.Pi*1.65, indigo)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func lerp(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{
		uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t),
		uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t),
		uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t),
		255,
	}
}

func line(img *image.NRGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx, dy := x1-x0, y1-y0
	steps := maxAbs(dx, dy)
	if steps == 0 {
		img.Set(x0, y0, c)
		return
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		for _, o := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
			img.Set(x+o[0], y+o[1], c)
		}
	}
}

func arc(img *image.NRGBA, cx, cy, r int, a0, a1 float64, c color.RGBA) {
	const steps = 120
	for i := 0; i <= steps; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(steps)
		x := cx + int(float64(r)*math.Cos(a))
		y := cy + int(float64(r)*math.Sin(a))
		for _, o := range [9][2]int{{0, 0}, {1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {-1, -1}, {1, -1}, {-1, 1}} {
			img.Set(x+o[0], y+o[1], c)
		}
	}
}

func maxAbs(a, b int) int {
	a2, b2 := abs(a), abs(b)
	if a2 > b2 {
		return a2
	}
	return b2
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
