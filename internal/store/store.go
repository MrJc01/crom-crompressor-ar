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
		o.Workers = runtime.GOMAXPROCS(0)
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
	idxOff int64
	idxIno uint64

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
	for _, d := range []string{dir, filepath.Join(dir, "seg"), filepath.Join(dir, "recipes")} {
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
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(opt.Level), zstd.WithEncoderConcurrency(opt.Workers), zstd.WithLowerEncoderMem(false))
	if err != nil {
		return nil, err
	}
	fast, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(opt.Workers))
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opt: opt, idx: map[Hash]loc{}, segs: map[uint32]*os.File{},
		cache: newFrameCache(opt.CacheBytes), enc: enc, fast: fast, dec: dec}
	// limpa receitas temporárias de uma gravação interrompida
	if tmps, _ := filepath.Glob(filepath.Join(dir, "recipes", "*", "*.tmp")); len(tmps) > 0 {
		for _, t := range tmps {
			os.Remove(t)
		}
	}
	s.mu.Lock()
	err = s.refreshLocked(false)
	s.mu.Unlock()
	if err != nil {
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

// refreshLocked lê do index.log o que outros processos gravaram. Com
// writer=true (flock exclusivo), um final cortado por queda é truncado.
func (s *Store) refreshLocked(writer bool) error {
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

// readFrame devolve o frame descomprimido (do cache, se houver).
func (s *Store) readFrame(seg uint32, off uint64) ([]byte, error) {
	key := frameKey{seg, off}
	if b := s.cache.get(key); b != nil {
		return b, nil
	}
	f, err := s.segReader(seg)
	if err != nil {
		return nil, err
	}
	hb := make([]byte, frameHdrLen)
	if _, err := f.ReadAt(hb, int64(off)); err != nil {
		return nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
	}
	hd, err := parseFrameHeader(hb)
	if err != nil {
		return nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
	}
	data := make([]byte, int(hd.compLen)+frameTailLen)
	if _, err := f.ReadAt(data, int64(off)+frameHdrLen); err != nil {
		return nil, fmt.Errorf("segmento %d @%d: %w", seg, off, err)
	}
	comp, tail := data[:hd.compLen], data[hd.compLen:]
	if crc32.Checksum(comp, crcTab) != uint32(tail[0])|uint32(tail[1])<<8|uint32(tail[2])<<16|uint32(tail[3])<<24 {
		return nil, fmt.Errorf("segmento %d @%d: checksum do frame não bate (dado corrompido)", seg, off)
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

func (s *Store) recipePath(h Hash) string {
	x := h.String()
	return filepath.Join(s.dir, "recipes", x[:2], x+".rcp")
}

// Has diz se o conteúdo h já está no depósito.
func (s *Store) Has(h Hash) bool {
	_, err := os.Stat(s.recipePath(h))
	return err == nil
}

// Recipe lê a receita do conteúdo h.
func (s *Store) Recipe(h Hash) (*Recipe, error) {
	b, err := os.ReadFile(s.recipePath(h))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("conteúdo %s não está no depósito", h.String()[:12])
		}
		return nil, err
	}
	return decodeRecipe(h, b)
}

// Contents lista os conteúdos guardados.
func (s *Store) Contents() ([]Hash, error) {
	var out []Hash
	root := filepath.Join(s.dir, "recipes")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".rcp") {
			return err
		}
		if h, err := ParseHash(strings.TrimSuffix(d.Name(), ".rcp")); err == nil {
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

// Remove apaga a receita de h. Os blocos que ficarem sem dono saem no GC.
func (s *Store) Remove(h Hash) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	lk, err := s.flock()
	if err != nil {
		return err
	}
	defer funlock(lk)
	err = os.Remove(s.recipePath(h))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------- gravação

// PutResult descreve uma gravação.
type PutResult struct {
	Hash       Hash  // SHA-256 do conteúdo inteiro
	Size       int64 // bytes originais
	Chunks     int   // blocos na receita
	NewChunks  int   // blocos que ainda não existiam
	NewBytes   int64 // bytes crus desses blocos
	StoredDisk int64 // bytes que entraram em disco (frames + índice + receita)
	RawFrames  int   // frames guardados crus (não comprimiam)
	Existed    bool  // o conteúdo inteiro já estava no depósito
}

type pendChunk struct {
	h      Hash
	off, n uint32
}

type frameJob struct {
	raw    []byte
	chunks []pendChunk
	codec  byte
	data   []byte
}

// PutFile grava o arquivo em path.
func (s *Store) PutFile(path string) (PutResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return PutResult{}, err
	}
	defer f.Close()
	return s.Put(f)
}

// Put lê r até o fim e grava o conteúdo no depósito.
func (s *Store) Put(r io.Reader) (res PutResult, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	lk, err := s.flock()
	if err != nil {
		return res, err
	}
	defer funlock(lk)
	s.mu.Lock()
	err = s.refreshLocked(true)
	s.mu.Unlock()
	if err != nil {
		return res, err
	}
	w, err := s.openSegWriter(false)
	if err != nil {
		return res, err
	}
	defer w.close()

	var (
		total   = sha256.New()
		recipe  Recipe
		pending = map[Hash]bool{}
		frame   = make([]byte, 0, s.opt.FrameSize+MaxChunk)
		fchunks []pendChunk
		batch   []*frameJob
		newRecs []byte
	)
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		s.compressBatch(batch)
		for _, j := range batch {
			seg, off, err := w.writeFrame(j.codec, len(j.raw), j.data)
			if err != nil {
				return err
			}
			if j.codec == codecRaw {
				res.RawFrames++
			}
			res.StoredDisk += int64(frameHdrLen + len(j.data) + frameTailLen)
			for _, c := range j.chunks {
				l := loc{seg: seg, frameOff: off, offInFrame: c.off, length: c.n}
				newRecs = append(newRecs, encodeIdx(c.h, l)...)
			}
		}
		batch = batch[:0]
		return nil
	}
	cutFrame := func() error {
		if len(frame) == 0 {
			return nil
		}
		batch = append(batch, &frameJob{raw: frame, chunks: fchunks})
		frame = make([]byte, 0, s.opt.FrameSize+MaxChunk)
		fchunks = nil
		if len(batch) >= s.opt.Workers {
			return flushBatch()
		}
		return nil
	}

	ch := newChunker(r)
	for {
		c, err := ch.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		total.Write(c)
		h := Hash(sha256.Sum256(c))
		recipe.Chunks = append(recipe.Chunks, RecipeChunk{Hash: h, Len: uint32(len(c))})
		recipe.Size += int64(len(c))
		if pending[h] || s.known(h) {
			continue
		}
		pending[h] = true
		res.NewChunks++
		res.NewBytes += int64(len(c))
		fchunks = append(fchunks, pendChunk{h: h, off: uint32(len(frame)), n: uint32(len(c))})
		frame = append(frame, c...)
		if len(frame) >= s.opt.FrameSize {
			if err := cutFrame(); err != nil {
				return res, err
			}
		}
	}
	if err := cutFrame(); err != nil {
		return res, err
	}
	if err := flushBatch(); err != nil {
		return res, err
	}
	copy(recipe.Hash[:], total.Sum(nil))
	res.Hash, res.Size, res.Chunks = recipe.Hash, recipe.Size, len(recipe.Chunks)

	// 1) blocos no disco  2) índice no disco  3) receita: só então o conteúdo existe
	if err := w.close(); err != nil {
		return res, err
	}
	if len(newRecs) > 0 {
		if err := s.appendIndex(newRecs); err != nil {
			return res, err
		}
		s.mu.Lock()
		err := s.refreshLocked(true) // lê o que acabou de gravar (mesmo caminho dos outros processos)
		s.mu.Unlock()
		if err != nil {
			return res, err
		}
		res.StoredDisk += int64(len(newRecs))
	}
	if s.Has(recipe.Hash) {
		res.Existed = true
		return res, nil
	}
	enc := encodeRecipe(&recipe)
	p := s.recipePath(recipe.Hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return res, err
	}
	if err := writeFileSync(p, enc); err != nil {
		return res, err
	}
	res.StoredDisk += int64(len(enc))
	return res, nil
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
			const probe = 64 << 10
			if len(j.raw) > 2*probe {
				// amostra rápida: se nem o nível mais rápido acha nada, nem tenta o caro
				p := s.fast.EncodeAll(j.raw[:probe], nil)
				if len(p) > probe*99/100 {
					return
				}
			}
			z := s.enc.EncodeAll(j.raw, make([]byte, 0, len(j.raw)/2))
			if len(z) < len(j.raw)*98/100 {
				j.codec, j.data = codecZstd, z
			}
		}(j)
	}
	wg.Wait()
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
	s.mu.Unlock()
	err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if fi, err := d.Info(); err == nil {
			st.DiskBytes += fi.Size()
		}
		switch {
		case strings.HasSuffix(p, ".rcp"):
			st.Contents++
		case strings.HasSuffix(p, ".seg"):
			st.Segments++
		}
		return nil
	})
	return st, err
}

// ---------------------------------------------------------------- utilidades

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
