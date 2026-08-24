// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- 测试替身 ---

// fakeFetcher 可编程 fetcher：记录调用与 URL 序列，可注入结果/错误。
type fakeFetcher struct {
	mu      sync.Mutex
	result  *FetchResult
	err     error
	calls   int
	urls    []string
	lastURL string
}

func (f *fakeFetcher) Fetch(ctx context.Context, url string) (*FetchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.urls = append(f.urls, url)
	f.lastURL = url
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFetcher) urlsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.urls))
	copy(out, f.urls)
	return out
}

type fakeUpserter struct {
	mu       sync.Mutex
	n        int
	err      error
	calls    int
	nVar     int
	errVar   error
	callsVar int
	varRows  []*domain.PriceVariant
}

func (u *fakeUpserter) UpsertPriceEntriesFromLiteLLM(ctx context.Context, rows []*domain.PriceEntry) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	return u.n, u.err
}

func (u *fakeUpserter) UpsertPriceVariantsFromLiteLLM(ctx context.Context, rows []*domain.PriceVariant) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.callsVar++
	u.varRows = rows
	return u.nVar, u.errVar
}

func (u *fakeUpserter) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *fakeUpserter) variantCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.callsVar
}

// fakeSettings 固定值 settings（mutex 保护，测试中可改）。
type fakeSettings struct {
	mu   sync.Mutex
	url  string
	cron string
}

func (s *fakeSettings) PriceSourceURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

func (s *fakeSettings) PriceSyncCron() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cron
}

// seqSettings 按调用序返回 URL/cron（确定性验证"每轮现读 → 变更下次生效"），
// 并记录读取次数。
type seqSettings struct {
	mu        sync.Mutex
	urls      []string
	crons     []string
	urlReads  int
	cronReads int
}

func (s *seqSettings) PriceSourceURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urlReads++
	if len(s.urls) == 0 {
		return "default-url"
	}
	u := s.urls[0]
	s.urls = s.urls[1:]
	return u
}

func (s *seqSettings) PriceSyncCron() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cronReads++
	if len(s.crons) == 0 {
		return "* * * * *"
	}
	c := s.crons[0]
	s.crons = s.crons[1:]
	return c
}

// fakeWait 可编程等待：按 seq 返回错误序列（不足则 lastErr，默认 nil）。
type fakeWait struct {
	mu      sync.Mutex
	seq     []error
	lastErr error
	times   []time.Duration
}

func (w *fakeWait) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.times = append(w.times, d)
	if len(w.seq) > 0 {
		e := w.seq[0]
		w.seq = w.seq[1:]
		if e != nil {
			return e
		}
		return nil
	}
	return w.lastErr
}

// waitBlock 阻塞至 ctx 取消（Start 测试用：cronLoop 保持存活）。
func waitBlock(ctx context.Context, d time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// fixedNow 固定时间基准（UTC，gronx 计算确定性）。
func fixedNow() time.Time { return time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC) }

// newTestWorker 构造测试 worker（fixed now + 可注入 wait）。
func newTestWorker(f *fakeFetcher, u *fakeUpserter, s SettingReader) *SyncWorker {
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: s, Reload: func() {}, Log: nil})
	w.now = fixedNow
	return w
}

// --- cron 调度数学 ---

// TestNextTrigger gronx 解析 + NextAfter 行为：合法表达式 → 下次触发时间；
// 空 cron → 默认兜底（0 3 * * *）；非法表达式 → 错误。
func TestNextTrigger(t *testing.T) {
	t.Run("daily 3am", func(t *testing.T) {
		w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{cron: "0 3 * * *"})
		next, err := w.nextTrigger()
		require.NoError(t, err)
		// now = 2026-08-08 10:00 UTC → 下次 08-09 03:00 UTC
		require.Equal(t, time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC), next)
	})

	t.Run("empty cron falls back to default", func(t *testing.T) {
		w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{cron: ""})
		next, err := w.nextTrigger()
		require.NoError(t, err, "快照缺失兜底默认 cron")
		require.Equal(t, time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC), next)
	})

	t.Run("every minute", func(t *testing.T) {
		w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{cron: "* * * * *"})
		next, err := w.nextTrigger()
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, 8, 8, 10, 1, 0, 0, time.UTC), next, "到点后下一分钟")
	})

	t.Run("before 3am same day", func(t *testing.T) {
		w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{cron: "0 3 * * *"})
		w.now = func() time.Time { return time.Date(2026, 8, 8, 2, 30, 0, 0, time.UTC) }
		next, err := w.nextTrigger()
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC), next, "当日 3 点")
	})

	t.Run("invalid cron", func(t *testing.T) {
		w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{cron: "not a cron"})
		_, err := w.nextTrigger()
		require.Error(t, err)
	})

	t.Run("nextDelay non-negative", func(t *testing.T) {
		w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{cron: "0 3 * * *"})
		d, err := w.nextDelay()
		require.NoError(t, err)
		require.Equal(t, 17*time.Hour, d, "10:00 → 次日 03:00 差 17h")
	})
}

// --- 同步执行路径（Sync/syncOnce） ---

func TestSyncSuccess(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m", Source: domain.PricingSourceLitellm}}}}
	u := &fakeUpserter{n: 1}
	reloads := 0
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"}, Reload: func() { reloads++ }, Log: nil})
	w.now = fixedNow

	require.NoError(t, w.Sync(context.Background()))
	require.Equal(t, 1, u.count(), "upsert 一次")
	require.Equal(t, 1, reloads, "成功后刷新快照")
	require.Equal(t, []string{"https://u"}, f.urlsSeen())
}

func TestSyncEmptySourceURL(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{}}
	u := &fakeUpserter{}
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: &fakeSettings{url: ""}, Log: nil})

	err := w.Sync(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "price_source_url")
	require.Zero(t, f.count(), "URL 缺失不拉取")
	require.Zero(t, u.count())
}

func TestSyncFetchFailureKeepsOldPrices(t *testing.T) {
	f := &fakeFetcher{err: errors.New("network down")}
	u := &fakeUpserter{}
	reloads := 0
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"}, Reload: func() { reloads++ }, Log: nil})

	require.Error(t, w.Sync(context.Background()))
	require.Zero(t, u.count(), "fetch 失败不落库")
	require.Zero(t, reloads, "fetch 失败不刷新快照（保留旧价格）")
}

func TestSyncPartialUpsertErrorStillReloads(t *testing.T) {
	// 仓库部分成功（分批独立事务）仍返回错误：已落库的批立即生效 → 刷新快照。
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m"}}}}
	u := &fakeUpserter{n: 100, err: errors.New("batch 2 failed")}
	reloads := 0
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"}, Reload: func() { reloads++ }, Log: nil})

	err := w.Sync(context.Background())
	require.Error(t, err, "部分失败错误透传（调用方告警）")
	require.Equal(t, 1, reloads, "部分落库仍刷新快照")
}

// TestSyncVariantLine 变体行独立落库
func TestSyncVariantLine(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m", Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm}}, Variants: []*domain.PriceVariant{{Model: "m", Seq: 1}}}}
	u := &fakeUpserter{n: 1, nVar: 1}
	reloads := 0
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"}, Reload: func() { reloads++ }, Log: nil})
	require.NoError(t, w.Sync(context.Background()))
	require.Equal(t, 1, u.variantCount())
}

// --- cron 循环 ---

// TestCronLoopFiresSyncOnSchedule 到点触发拉取：wait 首次返回 nil（timer 到点）
// → syncOnce 执行；第二次返回 ctx 取消 → 循环退出。共一次同步。
func TestCronLoopFiresSyncOnSchedule(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m"}}}}
	u := &fakeUpserter{n: 1}
	reloads := 0
	w := newTestWorker(f, u, &fakeSettings{url: "https://u", cron: "* * * * *"})
	w.reload = func() { reloads++ }
	fw := &fakeWait{seq: []error{nil, context.Canceled}}
	w.wait = fw.wait

	w.cronLoop(context.Background())
	require.Equal(t, 1, f.count(), "timer 到点触发一次拉取")
	require.Equal(t, 1, u.count())
	require.Equal(t, 1, reloads)
	require.Equal(t, []time.Duration{time.Minute, time.Minute}, fw.times, "cron * * * * * 每次等 1 分钟")
}

// TestCronLoopFailureNoCrash fetch 失败不崩：循环继续，等下个周期重试。
func TestCronLoopFailureNoCrash(t *testing.T) {
	f := &fakeFetcher{err: errors.New("boom")}
	u := &fakeUpserter{}
	reloads := 0
	w := newTestWorker(f, u, &fakeSettings{url: "https://u", cron: "* * * * *"})
	w.reload = func() { reloads++ }
	fw := &fakeWait{seq: []error{nil, nil, context.Canceled}}
	w.wait = fw.wait

	w.cronLoop(context.Background())
	require.Equal(t, 2, f.count(), "失败后下个周期继续重试")
	require.Zero(t, u.count(), "失败不落库")
	require.Zero(t, reloads, "失败保留旧快照")
}

// TestCronLoopSettingsChangeNextLoop 设置变更下次循环生效：URL/cron 每轮现读
// （seqSettings 按调用序返回），第二轮同步用新 URL；cron 读取次数 ≥ 循环次数。
func TestCronLoopSettingsChangeNextLoop(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m"}}}}
	u := &fakeUpserter{n: 1}
	ss := &seqSettings{urls: []string{"https://u1", "https://u2"}}
	w := newTestWorker(f, u, ss)
	fw := &fakeWait{seq: []error{nil, nil, context.Canceled}}
	w.wait = fw.wait

	w.cronLoop(context.Background())
	require.Equal(t, []string{"https://u1", "https://u2"}, f.urlsSeen(), "第二轮同步读到变更后的 URL")
	require.Equal(t, 2, f.count())
	require.GreaterOrEqual(t, ss.cronReads, 2, "cron 每轮现读（变更下次生效）")
}

// TestCronLoopInvalidCronRetry 非法 cron：Warn + 重试等待，不 panic、不拉取；
// 修正后恢复（seqSettings 第二轮返回合法表达式）。
func TestCronLoopInvalidCronRetry(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m"}}}}
	u := &fakeUpserter{n: 1}
	ss := &seqSettings{crons: []string{"bogus", "0 3 * * *"}}
	w := newTestWorker(f, u, ss)
	fw := &fakeWait{seq: []error{nil, context.Canceled}}
	w.wait = fw.wait

	w.cronLoop(context.Background())
	require.Zero(t, f.count(), "非法 cron 不触发拉取")
	require.Equal(t, []time.Duration{retryDelay, 17 * time.Hour}, fw.times,
		"非法 cron 走 1h 重试等待；修正后按新表达式调度")
}

// --- Start/Close 生命周期 ---

// TestStartAsyncFetchAndIdempotent Start 非阻塞 + 异步拉取一次 + 重复 Start
// 报错 + Close 幂等安全。
func TestStartAsyncFetchAndIdempotent(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m"}}}}
	u := &fakeUpserter{n: 1}
	w := newTestWorker(f, u, &fakeSettings{url: "https://u", cron: "* * * * *"})
	w.wait = waitBlock // cronLoop 保持存活至 ctx 取消

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, w.Start(ctx))
	require.Error(t, w.Start(ctx), "重复 Start 幂等报错")

	require.Eventually(t, func() bool { return f.count() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"启动异步拉取一次")
	require.NoError(t, w.Close(ctx), "Close 幂等安全")
	cancel()
}

// TestSyncWorkerInterface 满足 worker.Worker 契约签名（编译期断言）。
func TestSyncWorkerInterface(t *testing.T) {
	var _ interface {
		Name() string
		Start(ctx context.Context) error
		Close(ctx context.Context) error
	} = NewSyncWorker(SyncWorkerConfig{})
	require.Equal(t, "pricing-sync", NewSyncWorker(SyncWorkerConfig{}).Name())
}

// TestNewSyncWorkerDefaults 默认实现：wait 走真实 timer（ctx 取消立即返回）。
func TestNewSyncWorkerDefaults(t *testing.T) {
	w := NewSyncWorker(SyncWorkerConfig{Fetcher: &fakeFetcher{}, Repo: &fakeUpserter{}, Settings: &fakeSettings{}, Log: nil})
	require.NotNil(t, w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.wait(ctx, time.Hour), context.Canceled)
}
