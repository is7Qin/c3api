// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"slices"
	"sync"
	"time"
)

// winCounts 单账号单桶计数。
type winCounts struct {
	ok, err, t429 int
}

// windowSnapshot 近 N 秒聚合计数（total = ok + err + t429）。
type windowSnapshot struct {
	ok, err, t429 int
}

func (s windowSnapshot) total() int { return s.ok + s.err + s.t429 }

// ringBuckets 桶环完整历史桶数上限：覆盖窗口 = 完整桶数 × 粒度，取到 6 桶即止
// （60s 窗口 → 10s × 6；固定粒度近似，桶边界误差 ≤ 一个粒度）。
const ringBuckets = 6

// windowMap per 账号固定粒度滑动窗口（桶环）：
//
//	环 = ringBuckets 个完整历史桶 + 1 个当前桶（含 future 部分，恒为空）；
//	Add 推进时环上旧桶清零复用，Snapshot 按 (请求秒数, 当前时刻在桶内偏移)
//	向后聚合恰覆盖窗口的桶——任意 S 秒窗口必含 S 秒内全部事件（不早不晚）。
//
// 跨窗合并即聚合相邻桶；ok 计数仅在 trackOK（= 引擎 needsOK）时维护；
// 过期账号由 cleanup 按时间序扫描清理（防 map 泄漏）。
type windowMap struct {
	mu        sync.Mutex
	width     time.Duration         // 桶粒度（maxWindow 的整数切片）
	buckets   []map[int64]winCounts // 桶环，len = 完整历史桶 + 1
	cur       int                   // 当前桶下标（时间最新）
	nextAt    time.Time             // 环推进边界（对齐到粒度）
	trackOK   bool
	lastSeen  map[int64]time.Time // 账号最近事件时间（清理用）
	retention time.Duration
}

// reset 重建窗口：覆盖 maxWindow（多 window_seconds 规则取最大）、
// 粒度取整数切片档位；计数与 lastSeen 清零（Reload 语义）。
func (w *windowMap) reset(maxWindow time.Duration, trackOK bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if maxWindow <= 0 {
		maxWindow = time.Duration(defaultWindowSeconds) * time.Second
	}
	width, n := bucketWidth(maxWindow)
	w.width = width
	w.buckets = make([]map[int64]winCounts, n+1) // +1 当前桶
	for i := range w.buckets {
		w.buckets[i] = make(map[int64]winCounts)
	}
	w.cur = 0
	w.nextAt = time.Time{}
	w.trackOK = trackOK
	w.lastSeen = make(map[int64]time.Time)
	w.retention = 2 * time.Duration(len(w.buckets)) * width
	if w.retention < time.Minute {
		w.retention = time.Minute
	}
}

// bucketWidth 取 maxWindow 的整数切片粒度：从细到粗挑首个桶数 ≤ ringBuckets
// 的整档（60s → 10s × 6、30s → 5s × 6、90s → 15s × 6、20s → 5s × 4）；
// 无整档时退化为 maxWindow/6 向上取整到秒。
func bucketWidth(maxWindow time.Duration) (width time.Duration, n int) {
	for _, w := range []time.Duration{
		time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second,
		time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour,
	} {
		if maxWindow%w == 0 && maxWindow/w <= ringBuckets {
			return w, int(maxWindow / w)
		}
	}
	width = maxWindow / ringBuckets
	if maxWindow%ringBuckets != 0 {
		width += time.Second
	}
	if width < time.Second {
		width = time.Second
	}
	return width, int((maxWindow + width - 1) / width)
}

// Add 计入一个事件（occurred_at 所在桶）；早于环覆盖范围的事件丢弃。
func (w *windowMap) Add(ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buckets) == 0 {
		return
	}
	w.advanceTo(ev.OccurredAt)
	idx := w.bucketIdx(ev.OccurredAt)
	if idx < 0 {
		return // 太旧，超出环覆盖
	}
	if !w.trackOK && ev.Kind == KindOK {
		return // 无 ok 规则（needsOK=false）时不维护 ok 计数
	}
	c := w.buckets[idx][ev.AccountID]
	switch ev.Kind {
	case KindOK:
		c.ok++
	case Kind429:
		c.t429++
	// 错误事件桶：4xx/5xx/network 全部并入（枚举重构防呆——漏加则
	// count_error_ge/ratio_error_ge 静默失真；kind=error 已删除）。
	case Kind4xx, Kind5xx, KindNetwork:
		c.err++
	}
	w.buckets[idx][ev.AccountID] = c
	if last, ok := w.lastSeen[ev.AccountID]; !ok || ev.OccurredAt.After(last) {
		w.lastSeen[ev.AccountID] = ev.OccurredAt
	}
}

// Snapshot 近 seconds 秒的聚合计数（钳制到覆盖范围）。覆盖窗口的桶必含
// 窗口内全部事件；秒数小于桶内偏移时只含当前桶。
func (w *windowMap) Snapshot(aid int64, seconds int, at time.Time) windowSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out windowSnapshot
	if len(w.buckets) == 0 {
		return out
	}
	w.advanceTo(at)
	secs := time.Duration(seconds) * time.Second
	if secs <= 0 {
		return out
	}
	if coverage := time.Duration(len(w.buckets)) * w.width; secs > coverage {
		secs = coverage
	}
	// 窗口起点 at-secs 在桶内的偏移 frac：窗口覆盖 cur（部分）+ 之后 ⌈(secs-frac)/width⌉ 个完整桶
	frac := at.Sub(at.Truncate(w.width))
	n := 1
	if secs > frac {
		n += int((secs - frac + w.width - 1) / w.width)
	}
	if n > len(w.buckets) {
		n = len(w.buckets)
	}
	for i := 0; i < n; i++ {
		idx := w.cur - i
		if idx < 0 {
			idx += len(w.buckets)
		}
		if c, ok := w.buckets[idx][aid]; ok {
			out.ok += c.ok
			out.err += c.err
			out.t429 += c.t429
		}
	}
	return out
}

// cleanup 清理超过 retention 未见事件的账号：按 lastSeen 时间序扫描（升序），
// 遇新鲜账号即停（时间序剪枝）。
func (w *windowMap) cleanup(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lastSeen) == 0 {
		return
	}
	cutoff := now.Add(-w.retention)
	keys := make([]int64, 0, len(w.lastSeen))
	for a := range w.lastSeen {
		keys = append(keys, a)
	}
	slices.SortFunc(keys, func(a, b int64) int {
		ta, tb := w.lastSeen[a], w.lastSeen[b]
		switch {
		case ta.Before(tb):
			return -1
		case ta.After(tb):
			return 1
		}
		return 0
	})
	for _, a := range keys {
		if w.lastSeen[a].After(cutoff) {
			break
		}
		delete(w.lastSeen, a)
		for _, b := range w.buckets {
			delete(b, a)
		}
	}
}

// advanceTo 推进环到 t 所在桶。
func (w *windowMap) advanceTo(t time.Time) {
	if w.nextAt.IsZero() {
		w.nextAt = t.Truncate(w.width).Add(w.width)
		return
	}
	for !t.Before(w.nextAt) {
		w.advance()
	}
}

// advance 环进一格：进入的桶清零复用。
func (w *windowMap) advance() {
	w.cur = (w.cur + 1) % len(w.buckets)
	w.buckets[w.cur] = make(map[int64]winCounts)
	w.nextAt = w.nextAt.Add(w.width)
}

// bucketIdx t 所在桶下标；t 早于环覆盖范围返回 -1。
func (w *windowMap) bucketIdx(t time.Time) int {
	// ceil((nextAt-t)/width)，advanceTo 保证 t < nextAt 故 ≥ 1
	offset := int((w.nextAt.Sub(t) + w.width - 1) / w.width)
	if offset > len(w.buckets) {
		return -1
	}
	idx := (w.cur - offset + 1) % len(w.buckets)
	if idx < 0 {
		idx += len(w.buckets)
	}
	return idx
}
