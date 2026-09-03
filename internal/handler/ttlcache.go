// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"sync"
	"time"
)

// ttlCache 极简 TTL 缓存（overview/users-top 聚合面专用；spec 2026-08-14：
// TTL 语义 = dashboard 轮询频率下无陈旧感——overview 30s / users-top 2s）。
// 无 singleflight（P3 声明接受：dashboard 单消费者，重复聚合由 TTL 摊薄）。
// 键由调用方构造（含参数、请求时区与该时区日界——summary"今日"跨午夜滚转）。
// 过期惰性清除（get 命中过期项即删——条目数 ≤ 参数组合数，无后台清扫）。
type ttlCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]ttlItem
}

type ttlItem struct {
	expires time.Time
	value   any
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, items: make(map[string]ttlItem)}
}

// get 命中未过期缓存项 → (value, true)；未命中/已过期 → (nil, false)。
func (c *ttlCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		if ok {
			delete(c.items, key)
		}
		return nil, false
	}
	return it.value, true
}

// set 写入缓存项（TTL 从写入时刻起算）。
func (c *ttlCache) set(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = ttlItem{expires: time.Now().Add(c.ttl), value: v}
}
