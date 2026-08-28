// SPDX-License-Identifier: AGPL-3.0-or-later
package continuation

import (
	"container/heap"
	"sync"
	"time"
)

const (
	DefaultMaxEntries = 100_000
	DefaultMaxBytes   = 32 * 1024 * 1024
)

type l1Entry struct {
	key    string
	binding Binding
	expiry time.Time
	size   int
	index  int
}

type expiryHeap []*l1Entry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expiry.Before(h[j].expiry) }
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *expiryHeap) Push(x any) {
	e := x.(*l1Entry)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

type l1Cache struct {
	mu        sync.Mutex
	entries   map[string]*l1Entry
	heap      expiryHeap
	bytes     int
	maxEntries int
	maxBytes   int
	now       func() time.Time
}

func newL1(maxEntries, maxBytes int) *l1Cache {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &l1Cache{
		entries:    make(map[string]*l1Entry),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		now:        time.Now,
	}
}

func estimateSize(key string, b Binding) int {
	// rough: key + fingerprint.hex(64) + struct overhead 64
	return len(key) + len(b.Fingerprint) + 64 + 16
}

func (c *l1Cache) purgeExpiredLocked(now time.Time) {
	for c.heap.Len() > 0 {
		top := c.heap[0]
		if top.expiry.After(now) {
			break
		}
		heap.Pop(&c.heap)
		delete(c.entries, top.key)
		c.bytes -= top.size
	}
}

func (c *l1Cache) Get(key string) (Binding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return Binding{}, false
	}
	if !e.expiry.After(c.now()) {
		heap.Remove(&c.heap, e.index)
		delete(c.entries, key)
		c.bytes -= e.size
		return Binding{}, false
	}
	return e.binding, true
}

func (c *l1Cache) Put(key string, b Binding, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	exp := now.Add(ttl)
	c.purgeExpiredLocked(now)
	if old, ok := c.entries[key]; ok {
		c.bytes -= old.size
		old.binding = b
		old.expiry = exp
		old.size = estimateSize(key, b)
		c.bytes += old.size
		heap.Fix(&c.heap, old.index)
	} else {
		sz := estimateSize(key, b)
		e := &l1Entry{key: key, binding: b, expiry: exp, size: sz}
		c.entries[key] = e
		heap.Push(&c.heap, e)
		c.bytes += sz
	}
	for (len(c.entries) > c.maxEntries || c.bytes > c.maxBytes) && c.heap.Len() > 0 {
		ev := heap.Pop(&c.heap).(*l1Entry)
		delete(c.entries, ev.key)
		c.bytes -= ev.size
	}
}

func (c *l1Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *l1Cache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
