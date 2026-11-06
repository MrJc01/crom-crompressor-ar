// Package store é o depósito de blocos do cofre crom-ar.
//
// Cada conteúdo é cortado em blocos pelo conteúdo (CDC); um bloco igual é
// guardado uma única vez, venha de que arquivo vier. Blocos novos são
// agrupados em frames de ~1 MiB comprimidos com zstd (ou crus, se não
// comprimem: vídeo, jpg, zip), gravados em segmentos append-only. A receita
// de um conteúdo é a lista de seus blocos, e a leitura descomprime só os
// frames do trecho pedido.
package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

// Options ajusta o depósito. Zero = padrão.
type Options struct {
	FrameSize   int               // bytes crus por frame (padrão 1 MiB)
	SegmentSize int64             // tamanho para trocar de segmento (padrão 256 MiB)
	Level       zstd.EncoderLevel // padrão SpeedBestCompression
	CacheBytes  int64             // cache de frames descomprimidos (padrão 64 MiB)
	Workers     int               // compressão paralela (padrão GOMAXPROCS)
}

func (o *Options) normalize() {
	if o.FrameSize <= 0 {
		o.FrameSize = 1 << 20
	}
	if o.SegmentSize <= 0 {
		o.SegmentSize = 256 << 20
	}
	if o.Level == 0 {
		o.Level = zstd.SpeedBestCompression
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = 64 << 20
	}
	if o.Workers <= 0 {
		// cada encoder SpeedBestCompression guarda dezenas de MB: limitar a 4
		// mantém o pico de RAM baixo em PC modesto sem perder muita vazão
		o.Workers = min(runtime.GOMAXPROCS(0), 4)
	}
}

// Store é um depósito aberto. Seguro para uso concorrente; entre processos,
// as escritas são serializadas por flock.
type Store struct {
	dir string
	opt Options

	wmu    sync.Mutex   // um escritor por vez neste processo (entre processos: flock)
	mu     sync.RWMutex // protege idx/idxOff/idxIno
	idx    map[Hash]loc
	pend   map[Hash]*Recipe // receitas de um lote aberto, ainda não commitadas
	idxOff int64
	idxIno uint64

	rcp     map[Hash]rloc // receitas no recipes.log (protegido por mu)
	rcpOff  int64
	rcpIno  uint64
	rcpDead int  // registros mortos no log (o GC compacta)
	rcpBad  bool // log com registro corrompido: GC recusa

	segMu sync.Mutex
	segs  map[uint32]*os.File

	cache *frameCache
	enc   *zstd.Encoder
	fast  *zstd.Encoder
	dec   *zstd.Decoder
}

// Open abre (ou cria) o depósito em dir.
func Open(dir string, opt Options) (*Store, error) {
	opt.normalize()
	for _, d := range []string{dir, filepath.Join(dir, "seg")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	tag := filepath.Join(dir, "FORMAT")
	if b, err := os.ReadFile(tag); err == nil {
		if string(b) != formatTag {
			return nil, fmt.Errorf("depósito %s: formato desconhecido %q", dir, strings.TrimSpace(string(b)))
		}
	} else if err := writeFileSync(tag, []byte(formatTag)); err != nil {
		return nil, err
	}
	// janela do tamanho do frame: o zstd enxerga o frame inteiro
	win := 1 << 20
	for win < opt.FrameSize+MaxChunk && win < 64<<20 {
		win <<= 1
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(opt.Level), zstd.WithEncoderConcurrency(opt.Workers), zstd.WithWindowSize(win), zstd.WithLowerEncoderMem(true))
	if err != nil {
		return nil, err
	}
	fast, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(opt.Workers), zstd.WithWindowSize(2<<20), zstd.WithLowerEncoderMem(true))
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(min(runtime.GOMAXPROCS(0), 4)), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(64<<20))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opt: opt, idx: map[Hash]loc{}, pend: map[Hash]*Recipe{}, rcp: map[Hash]rloc{},
		segs: map[uint32]*os.File{}, cache: newFrameCache(opt.CacheBytes), enc: enc, fast: fast, dec: dec}
	s.mu.Lock()
	err = s.refreshLocked(false)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.migrateRecipeFiles(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close libera os arquivos abertos.
func (s *Store) Close() error {
	s.segMu.Lock()
	for id, f := range s.segs {
		f.Close()
		delete(s.segs, id)
	}
	s.segMu.Unlock()
	s.enc.Close()
	s.fast.Close()
	s.dec.Close()
	return nil
}

// Dir devolve a pasta do depósito.
func (s *Store) Dir() string { return s.dir }

// ---------------------------------------------------------------- trava

func (s *Store) flock() (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func funlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// ---------------------------------------------------------------- índice

func (s *Store) idxPath() string { return filepath.Join(s.dir, "index.log") }

// refreshLocked lê do index.log e do recipes.log o que outros processos
// gravaram. Com writer=true (flock exclusivo), um final cortado por queda é
// truncado.
func (s *Store) refreshLocked(writer bool) error {
	if err := s.refreshIndexLocked(writer); err != nil {
		return err
	}
	return s.refreshRecipesLocked(writer)
}

func (s *Store) refreshIndexLocked(writer bool) error {
	f, err := os.Open(s.idxPath())
	if os.IsNotExist(err) {
		s.idx, s.idxOff, s.idxIno = map[Hash]loc{}, 0, 0
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	ino := st.Sys().(*syscall.Stat_t).Ino
	if ino != s.idxIno || st.Size() < s.idxOff { // reescrito (GC): recarrega tudo
		s.idx, s.idxOff, s.idxIno = map[Hash]loc{}, 0, ino
	}
	if st.Size() == s.idxOff {
		return nil
	}
	buf := make([]byte, st.Size()-s.idxOff)
	if _, err := f.ReadAt(buf, s.idxOff); err != nil && err != io.EOF {
		return err
	}
	good := 0
	for good+idxRecLen <= len(buf) {
		h, l, ok := decodeIdx(buf[good : good+idxRecLen])
		if !ok {
			break
		}
		s.idx[h] = l
		good += idxRecLen
	}
	if good < len(buf) && writer {
		if err := os.Truncate(s.idxPath(), s.idxOff+int64(good)); err != nil {
			return err
		}
	}
	s.idxOff += int64(good)
	return nil
}

func (s *Store) lookup(h Hash) (loc, bool) {
	s.mu.RLock()
	l, ok := s.idx[h]
	s.mu.RUnlock()
	if ok {
		return l, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshLocked(false) != nil {
		return loc{}, false
	}
	l, ok = s.idx[h]
	return l, ok
}

// ---------------------------------------------------------------- segmentos

func (s *Store) segPath(id uint32) string {
	return filepath.Join(s.dir, "seg", fmt.Sprintf("%08d.seg", id))
}

func (s *Store) segIDs() ([]uint32, error) {
	ents, err := os.ReadDir(filepath.Join(s.dir, "seg"))
	if err != nil {
		return nil, err
	}
	var ids []uint32
	for _, e := range ents {
		n := strings.TrimSuffix(e.Name(), ".seg")
		if n == e.Name() {
			continue
		}
		if v, err := strconv.ParseUint(n, 10, 32); err == nil {
			ids = append(ids, uint32(v))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

type segWriter struct {
	s     *Store
	f     *os.File
	id    uint32
	size  int64
	dirty bool
}

func (s *Store) openSegWriter(forceNew bool) (*segWriter, error) {
	ids, err := s.segIDs()
	if err != nil {
		return nil, err
	}
	var id uint32 = 1
	if len(ids) > 0 {
		id = ids[len(ids)-1]
		if forceNew {
			id++
		}
	}
	f, err := os.OpenFile(s.segPath(id), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &segWriter{s: s, f: f, id: id, size: st.Size()}
	if w.size >= s.opt.SegmentSize {
		f.Close()
		return s.openSegWriter(true)
	}
	return w, nil
}

// writeFrame grava um frame e devolve o offset do cabeçalho.
func (w *segWriter) writeFrame(codec byte, rawLen int, data []byte) (uint32, uint64, error) {
	total := int64(frameHdrLen + len(data) + frameTailLen)
	if w.size > 0 && w.size+total > w.s.opt.SegmentSize {
		if err := w.close(); err != nil {
			return 0, 0, err
		}
		nw, err := w.s.openSegWriter(true)
		if err != nil {
			return 0, 0, err
		}
		*w = *nw
	}
	off := w.size
	var b bytes.Buffer
	b.Grow(int(total))
	b.Write(frameHeader(codec, rawLen, len(data)))
	b.Write(data)
	var crc [4]byte
	v := crc32.Checksum(data, crcTab)
	crc[0], crc[1], crc[2], crc[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	b.Write(crc[:])
	if _, err := w.f.WriteAt(b.Bytes(), off); err != nil {
		return 0, 0, err
	}
	w.size += total
	w.dirty = true
	return w.id, uint64(off), nil
}

func (w *segWriter) sync() error {
	if w.f == nil || !w.dirty {
		return nil
	}
	w.dirty = false
	return w.f.Sync()
}

func (w *segWriter) close() error {
	if w.f == nil {
		return nil
	}
	var err error
	if w.dirty {
		err = w.f.Sync()
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f = nil
	return err
}

func (s *Store) segReader(id uint32) (*os.File, error) {
	s.segMu.Lock()
	defer s.segMu.Unlock()
	if f, ok := s.segs[id]; ok {
		return f, nil
	}
	f, err := os.Open(s.segPath(id))
	if err != nil {
		return nil, err
	}
	s.segs[id] = f
	return f, nil
}

func (s *Store) dropSegReaders() {
	s.segMu.Lock()
	for id, f := range s.segs {
		f.Close()
		delete(s.segs, id)
	}
	s.segMu.Unlock()
}

// readRawFrame lê um frame como está gravado (comprimido), conferindo o CRC.
func (s *Store) readRawFrame(seg uint32, off uint64) (frameHdr, []byte, error) {
	f, err := s.segReader(seg)
	if err != nil {
		return frameHdr{}, nil, err
	}
	hb := make([]byte, frameHdrLen)
	if _, err := f.ReadAt(hb, int64(off)); err != nil {
		return frameHdr{}, nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
	}
	hd, err := parseFrameHeader(hb)
	if err != nil {
		return frameHdr{}, nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
	}
	data := make([]byte, int(hd.compLen)+frameTailLen)
	if _, err := f.ReadAt(data, int64(off)+frameHdrLen); err != nil {
		return frameHdr{}, nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
	}
	comp, tail := data[:hd.compLen], data[hd.compLen:]
	if crc32.Checksum(comp, crcTab) != uint32(tail[0])|uint32(tail[1])<<8|uint32(tail[2])<<16|uint32(tail[3])<<24 {
		return frameHdr{}, nil, fmt.Errorf("segmento %d @%d: checksum do frame não bate (dado corrompido)", seg, off)
	}
	return hd, comp, nil
}

// readFrame devolve o frame descomprimido (do cache, se houver).
func (s *Store) readFrame(seg uint32, off uint64) ([]byte, error) {
	key := frameKey{seg, off}
	if b := s.cache.get(key); b != nil {
		return b, nil
	}
	hd, comp, err := s.readRawFrame(seg, off)
	if err != nil {
		return nil, err
	}
	var raw []byte
	switch hd.codec {
	case codecRaw:
		raw = comp
	case codecZstd:
		raw, err = s.dec.DecodeAll(comp, make([]byte, 0, hd.rawLen))
		if err != nil {
			return nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
		}
	default:
		return nil, fmt.Errorf("segmento %d @%d: codec %d desconhecido", seg, off, hd.codec)
	}
	if len(raw) != int(hd.rawLen) {
		return nil, fmt.Errorf("segmento %d @%d: tamanho descomprimido não bate", seg, off)
	}
	s.cache.put(key, raw)
	return raw, nil
}

// chunk devolve os bytes de um bloco. Se o bloco mudou de lugar (GC em outro
// processo), recarrega o índice e tenta de novo.
func (s *Store) chunk(h Hash) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		l, ok := s.lookup(h)
		if !ok {
			return nil, fmt.Errorf("bloco %s não está no depósito", h.String()[:12])
		}
		fr, err := s.readFrame(l.seg, l.frameOff)
		if err == nil && int(l.offInFrame)+int(l.length) <= len(fr) {
			return fr[l.offInFrame : l.offInFrame+l.length], nil
		}
		if attempt == 0 {
			s.dropSegReaders()
			s.cache.clear()
			s.mu.Lock()
			s.idxIno = 0 // força recarga completa
			s.refreshLocked(false)
			s.mu.Unlock()
			continue
		}
		if err == nil {
			err = errors.New("posição do bloco fora do frame")
		}
		return nil, err
	}
	return nil, errors.New("inalcançável")
}

// ---------------------------------------------------------------- receitas

// ---------------------------------------------------------------- gravação

// PutResult descreve uma gravação. Num lote, StoredDisk só fica completo
// depois do Commit/Close (os frames juntam blocos de vários arquivos).
type PutResult struct {
	Hash       Hash  // SHA-256 do conteúdo inteiro
	Size       int64 // bytes originais
	Chunks     int   // blocos na receita
	NewChunks  int   // blocos que ainda não existiam
	NewBytes   int64 // bytes crus desses blocos
	StoredDisk int64 // bytes que esta gravação acrescentou ao disco (estimado por frame)
	RawFrames  int   // frames guardados crus (não comprimiam)
	Existed    bool  // o conteúdo inteiro já estava no depósito
}

type pendChunk struct {
	h      Hash
	off, n uint32
}

type contrib struct {
	r   *PutResult
	raw int
}

type frameJob struct {
	raw     []byte
	chunks  []pendChunk
	contrib []contrib
	codec   byte
	data    []byte
}

// PutFile grava o arquivo em path (lote de um arquivo só).
func (s *Store) PutFile(path string) (PutResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return PutResult{}, err
	}
	defer f.Close()
	return s.Put(f)
}

// Put lê r até o fim e grava o conteúdo no depósito (lote de um arquivo só).
func (s *Store) Put(r io.Reader) (PutResult, error) {
	b, err := s.Begin()
	if err != nil {
		return PutResult{}, err
	}
	res, err := b.Put(r)
	if err != nil {
		b.Abort()
		return PutResult{}, err
	}
	if err := b.Close(); err != nil {
		return PutResult{}, err
	}
	return *res, nil
}

// Batch grava muitos conteúdos de uma vez: os frames juntam blocos de vários
// arquivos (arquivos pequenos comprimem como num arquivo sólido), a
// compressão roda em paralelo entre arquivos e o disco é sincronizado uma vez
// por lote em vez de 3–4 fsync por arquivo. Ordem garantida no Commit:
// blocos no disco → índice no disco → receitas. Um conteúdo só existe para
// outros processos depois do Commit; antes disso, Has/Recipe o veem neste
// processo, mas os blocos podem ainda não ser legíveis.
type Batch struct {
	s       *Store
	lk      *os.File
	w       *segWriter
	frame   []byte
	fchunks []pendChunk
	fcont   []contrib
	jobs    []*frameJob
	pending map[Hash]bool // blocos esperando no frame/jobs (ainda fora do índice)
	recs    []byte        // registros de índice ainda fora do index.log
	recipes []*Recipe     // receitas esperando o Commit
	results []*PutResult
	dirty   int64 // bytes crus desde o último Commit
	done    bool
}

// Limites do lote antes de um Commit automático.
const (
	batchMaxBytes   = 1 << 30
	batchMaxRecipes = 5000
)

// Begin abre um lote. Enquanto aberto, outros escritores esperam.
func (s *Store) Begin() (*Batch, error) {
	s.wmu.Lock()
	lk, err := s.flock()
	if err != nil {
		s.wmu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	err = s.refreshLocked(true)
	s.mu.Unlock()
	if err != nil {
		funlock(lk)
		s.wmu.Unlock()
		return nil, err
	}
	w, err := s.openSegWriter(false)
	if err != nil {
		funlock(lk)
		s.wmu.Unlock()
		return nil, err
	}
	return &Batch{s: s, lk: lk, w: w, pending: map[Hash]bool{},
		frame: make([]byte, 0, s.opt.FrameSize+MaxChunk)}, nil
}

// Put corta, deduplica e enfileira o conteúdo de r.
func (b *Batch) Put(r io.Reader) (*PutResult, error) {
	s := b.s
	res := &PutResult{}
	b.results = append(b.results, res)
	recipe := &Recipe{}
	r, z := sniffZlib(r)
	var orig Hash
	if z != nil {
		// arquivo inteiro em zlib (objeto do git, .zz): guarda o conteúdo
		// descomprimido, que deduplica com os arquivos de verdade e comprime
		// melhor no frame; a leitura recria os bytes exatos.
		orig, recipe.Xform, recipe.Level = z.hash, xformZlib, byte(z.level)
		r = bytes.NewReader(z.plain)
	}
	total := sha256.New()
	ch := newChunker(r)
	for {
		c, err := ch.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		total.Write(c)
		h := Hash(sha256.Sum256(c))
		recipe.Chunks = append(recipe.Chunks, RecipeChunk{Hash: h, Len: uint32(len(c))})
		recipe.Size += int64(len(c))
		if b.pending[h] || s.known(h) {
			continue
		}
		b.pending[h] = true
		res.NewChunks++
		res.NewBytes += int64(len(c))
		b.fchunks = append(b.fchunks, pendChunk{h: h, off: uint32(len(b.frame)), n: uint32(len(c))})
		if n := len(b.fcont); n > 0 && b.fcont[n-1].r == res {
			b.fcont[n-1].raw += len(c)
		} else {
			b.fcont = append(b.fcont, contrib{r: res, raw: len(c)})
		}
		b.frame = append(b.frame, c...)
		b.dirty += int64(len(c))
		if len(b.frame) >= s.opt.FrameSize {
			if err := b.cutFrame(); err != nil {
				return nil, err
			}
		}
	}
	copy(recipe.Hash[:], total.Sum(nil))
	if z != nil {
		recipe.Hash, recipe.Size = orig, z.size
	}
	res.Hash, res.Size, res.Chunks = recipe.Hash, recipe.Size, len(recipe.Chunks)
	if s.Has(recipe.Hash) {
		res.Existed = true
	} else {
		b.recipes = append(b.recipes, recipe)
		s.mu.Lock()
		s.pend[recipe.Hash] = recipe
		s.mu.Unlock()
		res.StoredDisk += int64(len(recipeRecord(recipe)))
	}
	if b.dirty >= batchMaxBytes || len(b.recipes) >= batchMaxRecipes {
		if err := b.Commit(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (b *Batch) cutFrame() error {
	if len(b.frame) == 0 {
		return nil
	}
	b.jobs = append(b.jobs, &frameJob{raw: b.frame, chunks: b.fchunks, contrib: b.fcont})
	b.frame = make([]byte, 0, b.s.opt.FrameSize+MaxChunk)
	b.fchunks, b.fcont = nil, nil
	if len(b.jobs) >= b.s.opt.Workers {
		return b.writeJobs()
	}
	return nil
}

func (b *Batch) writeJobs() error {
	if len(b.jobs) == 0 {
		return nil
	}
	s := b.s
	s.compressBatch(b.jobs)
	for _, j := range b.jobs {
		seg, off, err := b.w.writeFrame(j.codec, len(j.raw), j.data)
		if err != nil {
			return err
		}
		disk := frameHdrLen + len(j.data) + frameTailLen
		disk += idxRecLen * len(j.chunks)
		for _, c := range j.contrib {
			c.r.StoredDisk += int64(disk) * int64(c.raw) / int64(len(j.raw))
			if j.codec == codecRaw {
				c.r.RawFrames++
			}
		}
		for _, c := range j.chunks {
			l := loc{seg: seg, frameOff: off, offInFrame: c.off, length: c.n}
			b.recs = append(b.recs, encodeIdx(c.h, l)...)
		}
	}
	b.jobs = b.jobs[:0]
	return nil
}

// Commit torna duráveis e visíveis os conteúdos gravados até aqui.
func (b *Batch) Commit() error {
	s := b.s
	if err := b.cutFrame(); err != nil {
		return err
	}
	if err := b.writeJobs(); err != nil {
		return err
	}
	if len(b.recs) == 0 && len(b.recipes) == 0 {
		return nil
	}
	// 1) blocos no disco
	if err := b.w.sync(); err != nil {
		return err
	}
	// 2) índice no disco
	if len(b.recs) > 0 {
		if err := s.appendIndex(b.recs); err != nil {
			return err
		}
		s.mu.Lock()
		err := s.refreshLocked(true)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		b.recs = b.recs[:0]
		for h := range b.pending {
			delete(b.pending, h)
		}
	}
	// 3) receitas: um append no recipes.log + fsync
	if len(b.recipes) > 0 {
		var buf []byte
		for _, r := range b.recipes {
			buf = append(buf, recipeRecord(r)...)
		}
		if err := s.appendRecipes(buf); err != nil {
			return err
		}
		s.mu.Lock()
		err := s.refreshLocked(true)
		for _, r := range b.recipes {
			delete(s.pend, r.Hash)
		}
		s.mu.Unlock()
		if err != nil {
			return err
		}
		b.recipes = b.recipes[:0]
	}
	b.dirty = 0
	return nil
}

// Close faz o Commit final e libera o depósito para outros escritores.
func (b *Batch) Close() error {
	if b.done {
		return nil
	}
	err := b.Commit()
	b.release()
	return err
}

// Abort descarta o que não foi commitado (os blocos já gravados viram lixo
// que o GC recolhe; nenhuma receita aponta para eles).
func (b *Batch) Abort() {
	if b.done {
		return
	}
	s := b.s
	s.mu.Lock()
	for _, r := range b.recipes {
		delete(s.pend, r.Hash)
	}
	s.mu.Unlock()
	b.release()
}

func (b *Batch) release() {
	b.done = true
	b.w.close()
	funlock(b.lk)
	b.s.wmu.Unlock()
}

// compressBatch comprime os frames em paralelo. Frame que não comprime
// (vídeo, jpg, zip, dado cifrado) fica cru: não gasta CPU na leitura.
func (s *Store) compressBatch(batch []*frameJob) {
	var wg sync.WaitGroup
	for _, j := range batch {
		wg.Add(1)
		go func(j *frameJob) {
			defer wg.Done()
			j.codec, j.data = codecRaw, j.raw
			if !s.worthCompressing(j.raw) {
				return
			}
			z := s.enc.EncodeAll(j.raw, make([]byte, 0, len(j.raw)/2))
			if len(z) < len(j.raw)*98/100 {
				j.codec, j.data = codecZstd, z
			}
		}(j)
	}
	wg.Wait()
}

// worthCompressing amostra 4 pontos do frame (início, 1/3, 2/3, fim) com o
// nível mais rápido e, se nenhum ganhar, o frame inteiro. Um frame junta
// blocos de vários arquivos: um jpg no começo não quer dizer que o resto
// (texto, json) não comprima. Só pula o nível caro se nada ganhar.
func (s *Store) worthCompressing(raw []byte) bool {
	const probe = 32 << 10
	const spots = 4
	if len(raw) <= 2*spots*probe {
		return true
	}
	step := (len(raw) - probe) / (spots - 1)
	var buf []byte
	for i := 0; i < spots; i++ {
		at := i * step
		buf = s.fast.EncodeAll(raw[at:at+probe], buf[:0])
		if len(buf) < probe*97/100 {
			return true
		}
	}
	// As amostras não acham nada, mas a repetição pode estar a mais de 32 KB
	// de distância dentro do frame (blocos de .sst, áudio, binários): no
	// crom-openllm isso deixava 330 MB crus que o zstd-11 levaria a 254 MB.
	// O nível rápido no frame inteiro decide; em dado aleatório ele é barato.
	buf = s.fast.EncodeAll(raw, make([]byte, 0, len(raw)))
	return len(buf) < len(raw)*99/100
}

// appendIndex grava no fim do index.log (com flock: o fim é sempre um
// registro inteiro, porque refreshLocked(true) truncou restos de queda).
func (s *Store) appendIndex(recs []byte) error {
	f, err := os.OpenFile(s.idxPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(recs); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Store) known(h Hash) bool {
	s.mu.RLock()
	_, ok := s.idx[h]
	s.mu.RUnlock()
	return ok
}

// ---------------------------------------------------------------- leitura

// File é um conteúdo aberto para leitura aleatória (io.ReaderAt).
type File struct {
	s      *Store
	r      *Recipe
	starts []int64 // offset de início de cada bloco

	// conteúdo com transformação (zlib): recriado inteiro na primeira leitura
	once  sync.Once
	plain []byte
	perr  error
}

// Open abre o conteúdo h para leitura.
func (s *Store) Open(h Hash) (*File, error) {
	r, err := s.Recipe(h)
	if err != nil {
		return nil, err
	}
	starts := make([]int64, len(r.Chunks))
	var off int64
	for i, c := range r.Chunks {
		starts[i] = off
		off += int64(c.Len)
	}
	return &File{s: s, r: r, starts: starts}, nil
}

// Size devolve o tamanho original.
func (f *File) Size() int64 { return f.r.Size }

// ReadAt lê len(p) bytes a partir de off, descomprimindo só o necessário.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("offset negativo")
	}
	if off >= f.r.Size {
		return 0, io.EOF
	}
	if f.r.Xform != xformNone {
		if err := f.materialize(); err != nil {
			return 0, err
		}
		n := copy(p, f.plain[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}
	i := sort.Search(len(f.starts), func(i int) bool { return f.starts[i] > off }) - 1
	n := 0
	for n < len(p) && i < len(f.r.Chunks) {
		data, err := f.s.chunk(f.r.Chunks[i].Hash)
		if err != nil {
			return n, err
		}
		in := off + int64(n) - f.starts[i]
		n += copy(p[n:], data[in:])
		i++
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteTo grava o conteúdo inteiro em w, conferindo cada bloco e o SHA-256 final.
func (f *File) WriteTo(w io.Writer) (int64, error) {
	if f.r.Xform != xformNone {
		if err := f.materialize(); err != nil {
			return 0, err
		}
		n, err := w.Write(f.plain)
		return int64(n), err
	}
	total := sha256.New()
	var n int64
	for _, c := range f.r.Chunks {
		data, err := f.s.chunk(c.Hash)
		if err != nil {
			return n, err
		}
		if Hash(sha256.Sum256(data)) != c.Hash {
			return n, fmt.Errorf("bloco %s corrompido", c.Hash.String()[:12])
		}
		total.Write(data)
		m, err := w.Write(data)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	if !bytes.Equal(total.Sum(nil), f.r.Hash[:]) {
		return n, errors.New("SHA-256 do conteúdo não bate")
	}
	return n, nil
}

// Verify relê o conteúdo h inteiro e confere blocos e SHA-256.
func (s *Store) Verify(h Hash) error {
	f, err := s.Open(h)
	if err != nil {
		return err
	}
	_, err = f.WriteTo(io.Discard)
	return err
}

// Extract grava o conteúdo h em path (tmp + rename, só se o SHA-256 bater).
func (s *Store) Extract(h Hash, path string, mode os.FileMode) error {
	f, err := s.Open(h)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".crom-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := f.WriteTo(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if mode != 0 {
		os.Chmod(tmp.Name(), mode)
	}
	return os.Rename(tmp.Name(), path)
}

// ---------------------------------------------------------------- números

// Stats resume o depósito.
type Stats struct {
	Contents     int   `json:"contents"`
	UniqueChunks int   `json:"unique_chunks"`
	Segments     int   `json:"segments"`
	DiskBytes    int64 `json:"disk_bytes"` // tudo em disco: segmentos, índice, receitas
}

// Stats calcula os números do depósito.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	s.mu.Lock()
	s.refreshLocked(false)
	st.UniqueChunks = len(s.idx)
	st.Contents = len(s.rcp)
	s.mu.Unlock()
	err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if fi, err := d.Info(); err == nil {
			st.DiskBytes += allocated(fi)
		}
		if strings.HasSuffix(p, ".seg") {
			st.Segments++
		}
		return nil
	})
	return st, err
}

// ---------------------------------------------------------------- utilidades

// allocated é o que o arquivo ocupa de fato no disco (blocos alocados), não
// o tamanho aparente: arquivo de 100 bytes gasta um bloco inteiro.
func allocated(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// syncFS sincroniza o sistema de arquivos do depósito (uma chamada no lugar
// de um fsync por arquivo).
func (s *Store) syncFS() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Syncfs(int(d.Fd()))
}

func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Remove apaga a receita de h usando a trava que o lote já tem.
func (b *Batch) Remove(h Hash) error {
	s := b.s
	s.mu.Lock()
	delete(s.pend, h)
	s.mu.Unlock()
	for i, r := range b.recipes {
		if r.Hash == h {
			b.recipes = append(b.recipes[:i], b.recipes[i+1:]...)
			break
		}
	}
	return s.removeRecipeLocked(h)
}
