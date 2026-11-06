package store

import (
	"container/list"
	"sync"
)

type frameKey struct {
	seg uint32
	off uint64
}

// frameCache guarda frames descomprimidos em RAM (LRU por bytes).
// Ler um vídeo do começo ao fim, ou um SQLite aos saltos, reaproveita o frame
// em vez de descomprimir de novo a cada read de 128 KB do FUSE.
type frameCache struct {
	mu    sync.Mutex
	max   int64
	size  int64
	ll    *list.List
	items map[frameKey]*list.Element
}

type cacheItem struct {
	k frameKey
	v []byte
}

func newFrameCache(max int64) *frameCache {
	return &frameCache{max: max, ll: list.New(), items: map[frameKey]*list.Element{}}
}

func (c *frameCache) get(k frameKey) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*cacheItem).v
	}
	return nil
}

func (c *frameCache) put(k frameKey, v []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = c.ll.PushFront(&cacheItem{k, v})
	c.size += int64(len(v))
	for c.size > c.max && c.ll.Len() > 1 {
		e := c.ll.Back()
		it := e.Value.(*cacheItem)
		c.ll.Remove(e)
		delete(c.items, it.k)
		c.size -= int64(len(it.v))
	}
}

func (c *frameCache) clear() {
	c.mu.Lock()
	c.ll.Init()
	c.items = map[frameKey]*list.Element{}
	c.size = 0
	c.mu.Unlock()
}
