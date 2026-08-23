// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// stats.ttft TTL 缓存单元测试（spec-ttft-cache-2026-08-23 §三）。
// 内部测试包：直接构造 newTTFTCache 注入时钟/上限桩，不经 Service/Store 组装。

package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// newTestCache 固定时钟起点（惯例：time.Date(2026,8,...) 注入）+ 标准容量。
func newTestCache(ttl time.Duration) *ttftCache {
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	return &ttftCache{
		ttl:    ttl,
		maxEnt: ttftCacheMaxEnt,
		now:    func() time.Time { return base },
		done:   map[string]ttftCacheEntry{},
		calls:  map[string]*ttftCacheCall{},
	}
}

// countingFn 可计数被测函数：可选 gate 屏障阻塞首段执行（channel 屏障替代
// sleep 的仓库惯例），ret/err 为放行后的返回。
type countingFn struct {
	calls atomic.Int32
	ret   *domain.TTFTSummary
	err   error
	gate  chan struct{}
}

func (f *countingFn) fn() func() (*domain.TTFTSummary, error) {
	return func() (*domain.TTFTSummary, error) {
		f.calls.Add(1)
		if f.gate != nil {
			<-f.gate
		}
		return f.ret, f.err
	}
}

func TestTTFTCache_HitSkipsFn(t *testing.T) {
	c := newTestCache(30 * time.Second)
	f := &countingFn{ret: &domain.TTFTSummary{Count: 7}}
	fn := f.fn()

	s1, err := c.fetch("acct|42||100|200", fn)
	require.NoError(t, err)
	require.Equal(t, int64(7), s1.Count)

	s2, err := c.fetch("acct|42||100|200", fn)
	require.NoError(t, err)
	require.Same(t, s1, s2, "命中缓存应返回同一结果对象")
	require.Equal(t, int32(1), f.calls.Load(), "同键第二次调用不得再执行 fn")
}

func TestTTFTCache_TTLExpiryRefetches(t *testing.T) {
	c := newTestCache(30 * time.Second)
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	elapsed := base
	c.now = func() time.Time { return elapsed }

	f := &countingFn{ret: &domain.TTFTSummary{Count: 1}}
	fn := f.fn()

	_, err := c.fetch("k", fn)
	require.NoError(t, err)

	elapsed = base.Add(29 * time.Second) // 未过期 → 命中
	_, err = c.fetch("k", fn)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.calls.Load(), "TTL 内命中")

	elapsed = base.Add(31 * time.Second) // 过期 → 重取
	_, err = c.fetch("k", fn)
	require.NoError(t, err)
	require.Equal(t, int32(2), f.calls.Load(), "过期后必须重执行 fn")
}

func TestTTFTCache_ErrorsNotCached(t *testing.T) {
	c := newTestCache(30 * time.Second)
	f := &countingFn{err: ErrInvalidInput}
	fn := f.fn()

	_, err := c.fetch("k", fn)
	require.ErrorIs(t, err, ErrInvalidInput)

	f.err = nil
	f.ret = &domain.TTFTSummary{Count: 3}
	s, err := c.fetch("k", fn)
	require.NoError(t, err, "失败不入缓存：次调必须重新执行 fn")
	require.Equal(t, int64(3), s.Count)
	require.Equal(t, int32(2), f.calls.Load())

	_, err = c.fetch("k", fn)
	require.NoError(t, err)
	require.Equal(t, int32(2), f.calls.Load(), "成功后才进缓存")
}

func TestTTFTCache_DistinctKeysIsolated(t *testing.T) {
	c := newTestCache(30 * time.Second)
	var acctCalls, userCalls atomic.Int32
	acctFn := func() (*domain.TTFTSummary, error) {
		acctCalls.Add(1)
		return &domain.TTFTSummary{Count: 10}, nil
	}
	userFn := func() (*domain.TTFTSummary, error) {
		userCalls.Add(1)
		return &domain.TTFTSummary{Count: 20}, nil
	}

	s1, err := c.fetch("account|1||100|200", acctFn)
	require.NoError(t, err)
	s2, err := c.fetch("user|2||100|200", userFn)
	require.NoError(t, err)
	require.Equal(t, int64(10), s1.Count)
	require.Equal(t, int64(20), s2.Count)

	_, err = c.fetch("account|1||100|200", acctFn)
	require.NoError(t, err)
	_, err = c.fetch("user|2||100|200", userFn)
	require.NoError(t, err)
	require.Equal(t, int32(1), acctCalls.Load(), "键隔离：互不污染命中")
	require.Equal(t, int32(1), userCalls.Load())
}

func TestTTFTCache_WaitersGetFnError(t *testing.T) {
	// RG-B1 探测场景：并发等待方经 close(done) 的 happens-before 读取结果——
	// 若发布顺序颠倒（先 close 后写字段）此用例在 -race 下必炸。
	c := newTestCache(30 * time.Second)
	const n = 6
	f := &countingFn{err: ErrInvalidInput, gate: make(chan struct{})}
	fn := f.fn()

	arrived := make([]chan struct{}, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		arrived[i] = make(chan struct{})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			close(arrived[i])
			_, errs[i] = c.fetch("err-key", fn)
		}(i)
	}
	for _, a := range arrived {
		<-a
	}
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.calls) == 1
	}, time.Second, time.Millisecond)

	close(f.gate)
	wg.Wait()
	for i := 0; i < n; i++ {
		require.ErrorIs(t, errs[i], ErrInvalidInput, "等待方必须收到同一错误")
	}
	require.Equal(t, int32(1), f.calls.Load())
	require.Zero(t, len(c.done), "失败不得入缓存")
}

func TestTTFTCache_InflightDedup(t *testing.T) {
	c := newTestCache(30 * time.Second)
	const n = 8
	f := &countingFn{ret: &domain.TTFTSummary{Count: 9}, gate: make(chan struct{})}
	fn := f.fn()

	arrived := make([]chan struct{}, n)
	var wg sync.WaitGroup
	results := make([]*domain.TTFTSummary, n)
	for i := 0; i < n; i++ {
		arrived[i] = make(chan struct{})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			close(arrived[i]) // 先于 fetch 的到达信号
			r, _ := c.fetch("hot", fn)
			results[i] = r
		}(i)
	}
	for _, a := range arrived {
		<-a // 全部已到达 fetch 调用点
	}
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.calls) == 1
	}, time.Second, time.Millisecond, "并发同键应合并为单个 inflight")

	close(f.gate) // 放行唯一执行者
	wg.Wait()
	require.Equal(t, int32(1), f.calls.Load(), "雷鸣群必须去重为一次落库")
	for i := 0; i < n; i++ {
		require.NotNil(t, results[i])
		require.Equal(t, int64(9), results[i].Count)
	}
}

func TestTTFTCache_MaxEntriesReset(t *testing.T) {
	c := newTestCache(30 * time.Second)
	c.maxEnt = 4

	mk := func(key string, count int64) (func() (*domain.TTFTSummary, error), *atomic.Int32) {
		var calls atomic.Int32
		return func() (*domain.TTFTSummary, error) {
				calls.Add(1)
				return &domain.TTFTSummary{Count: count}, nil
			},
			&calls
	}
	fn0, c0 := mk("k0", 0)
	fn1, c1 := mk("k1", 1)
	fn2, c2 := mk("k2", 2)
	fn3, c3 := mk("k3", 3)
	fn4, c4 := mk("k4", 4)

	for _, pair := range []struct {
		key string
		fn  func() (*domain.TTFTSummary, error)
	}{{"k0", fn0}, {"k1", fn1}, {"k2", fn2}, {"k3", fn3}} {
		_, err := c.fetch(pair.key, pair.fn)
		require.NoError(t, err)
	}

	_, err := c.fetch("k4", fn4) // 第 5 键触发整体重置
	require.NoError(t, err)

	_, err = c.fetch("k0", fn0)
	require.NoError(t, err)
	require.Equal(t, int32(2), c0.Load(), "重置后旧键不再命中")

	_, err = c.fetch("k4", fn4)
	require.NoError(t, err)
	require.Equal(t, int32(1), c4.Load(), "重置轮写入的新键正常命中")

	require.Equal(t, int32(1), c1.Load(), "被重置键保持各自首次执行计数")
	require.Equal(t, int32(1), c2.Load())
	require.Equal(t, int32(1), c3.Load())
}
