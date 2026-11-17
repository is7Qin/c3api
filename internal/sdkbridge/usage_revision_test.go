// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// S4a：usage 缓存的**代际绑定**（门禁 7/8/15/16/27）+ 单飞去重 + 新鲜度。
//
// 这一组用例只钉"观测属于哪一代身份 K"和"哪份观测更新鲜"这两件事——它们是
// 设计 §5.1–§5.3 的全部判决输入。所有交错都靠"上游闸门 + 到达信号"编排，
// 时间推进靠注入时钟（`Codex.usageNow`），**不 sleep 长时长**（R2-Q5 的
// "不撕裂/不误命中"类断言在 -race 下同样必须成立）。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// usageRevBody 一扇"未打满"的窗口（used_percent 用来区分不同次拉取），reset 在
// 15 分钟后（落在 1.25×1h 的卫生上界内，不影响判决）。
func usageRevBody(usedPercent float64) string {
	return usageWindowBody(usedPercent, 3600, time.Now().Add(15*time.Minute).Unix(), 900)
}

// usageCredRev usage 测试凭据 + 身份代际 K（S4a 的缓存代际维度）。
func usageCredRev(accountID, k int64) *domain.AccountCredential {
	c := usageCred(accountID, "")
	c.IdentityRevision = k
	return c
}

// usageGate 一次上游请求的闸门：`arrived` 在 handler 收包时关闭（测试据此确认
// "这个请求确实在上游 in-flight"），`release` 由测试关闭后才写响应。
type usageGate struct {
	arrived  chan struct{}
	release  chan struct{}
	released bool // 受 usageRevUpstream.mu 保护（allow 可重入）
	status   int
	body     string
}

// usageRevUpstream 可编排的 usage 上游：第 i 个**到达**的请求拿第 i 个闸门与
// 响应体。交错顺序因此完全确定（禁 sleep）。
type usageRevUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  int
	con    int
	maxCon int
	gates  []*usageGate
}

func newUsageRevUpstream(t *testing.T, steps ...codexUpstreamStep) *usageRevUpstream {
	t.Helper()
	u := &usageRevUpstream{}
	for _, s := range steps {
		u.gates = append(u.gates, &usageGate{
			arrived: make(chan struct{}),
			release: make(chan struct{}),
			status:  s.status,
			body:    s.body,
		})
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		u.mu.Lock()
		i := u.calls
		u.calls++
		u.con++
		if u.con > u.maxCon {
			u.maxCon = u.con
		}
		u.mu.Unlock()
		if i >= len(u.gates) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected usage call"}}`))
			return
		}
		g := u.gates[i]
		close(g.arrived)
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
		u.mu.Lock()
		u.con--
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		_, _ = w.Write([]byte(g.body))
	}))
	// 清理必须**先**放行全部闸门再关服务（httptest.Close 会等在途 handler）。
	t.Cleanup(func() {
		u.allowAll()
		u.srv.Close()
	})
	return u
}

// allow 放行第 i 个请求（可重入：失败路径的 t.Fatal 之后 cleanup 会再放一次）。
func (u *usageRevUpstream) allow(i int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.gates[i].released {
		u.gates[i].released = true
		close(u.gates[i].release)
	}
}

func (u *usageRevUpstream) allowAll() {
	for i := range u.gates {
		u.allow(i)
	}
}

// waitArrived 等第 i 个请求到达上游（超时兜底，防编排写错时测试挂死）。
func (u *usageRevUpstream) waitArrived(t *testing.T, i int) {
	t.Helper()
	select {
	case <-u.gates[i].arrived:
	case <-time.After(5 * time.Second):
		t.Fatalf("第 %d 个 usage 请求未到达上游", i)
	}
}

func (u *usageRevUpstream) callsN() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *usageRevUpstream) maxConcurrent() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.maxCon
}

// usageFlightCase 一次 usage 拉取编排的固定底座：可编排上游 + 共享 ClientTable +
// n 个适配器（各自独立在飞表）+ 注入时钟。
//
// 契约（改它之前先读——下面四条都是既有断言的成立前提，不是风格偏好）：
//   - **时钟是参数**：now 非 nil 时注入它创建的**每一个**适配器
//     （EqualTimestamp 要求 a1/a2 同一固定时钟，漏注入一个就判不出"相等时刻
//     保留缓存"）；nil = 用真实时钟。
//   - **n=2 时"共享 ClientTable + 各自 flights"是参数**，不许简化成单适配器：
//     共享表才能让两个适配器落在**同一条目**上，各自 flights 才能让同 K 的两个
//     上游观测同时在途。简化后第二个请求会并到第一个 flight 上，
//     waitArrived(t,1) 直接 5s 超时——症状会被误读成环境问题。
//   - readUsageCache 保持"同一 usageMu 读 8 字段"的联合快照，不许拆 per-field
//     getter：本组钉的就是"数据/时刻/代际/失败三元组一起翻页"。
//   - usageWaitProbe 保留（follower 的等待点靠它，不许换成 sleep）。
type usageFlightCase struct {
	up       *usageRevUpstream
	tbl      *ClientTable
	adapters []*Codex
}

// newUsageFlightCase 造一次编排：上游按 steps 逐个放行，适配器共享一张表。
func newUsageFlightCase(t *testing.T, now func() time.Time, n int, steps ...codexUpstreamStep) *usageFlightCase {
	t.Helper()
	c := &usageFlightCase{up: newUsageRevUpstream(t, steps...), tbl: NewClientTable()}
	pool := testEgressPool(newOfficialRewriteTransport(t, c.up.srv.URL))
	for range n {
		a := NewCodex(nil, pool, c.tbl, RotationDeps{})
		if now != nil {
			a.usageNow = now
		}
		c.adapters = append(c.adapters, a)
	}
	return c
}

// a 取第 i 个适配器（越界即编排写错：直接判死，不要静默给 nil）。
func (c *usageFlightCase) a(i int) *Codex {
	if i >= len(c.adapters) {
		panic("usageFlightCase: adapter index out of range")
	}
	return c.adapters[i]
}

// usageFetch 一次在途拉取的句柄：done 关闭后 snap/err 可读（-race 安全：写发生
// 在 close(done) 之前，读发生在 <-done 之后）。
type usageFetch struct {
	done chan struct{}
	snap *domain.CodexUsageSnapshot
	err  error
}

func (f *usageFetch) wait() { <-f.done }

// startUsageFetch 起一个拉取并立刻返回句柄。禁止在测试里散写
// `var s *...; var e error; done := make(chan struct{}); go func(){...}()`：
// 那样每个用例都要复制一遍"写者/读者之间只靠 close(done) 建 happens-before"，
// 漏一处就是 -race 下的假红或假绿。
func startUsageFetch(a *Codex, ctx context.Context, cred *domain.AccountCredential) *usageFetch {
	f := &usageFetch{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		f.snap, f.err = a.GetUsageSnapshot(ctx, cred)
	}()
	return f
}

// usageCacheView 一次读面上的完整缓存状态（同一 usageMu 临界区读出——"不撕裂"
// 断言的观测面：数据、时刻、代际标签、失败三元组必须彼此一致）。
type usageCacheView struct {
	hasSnap bool
	percent int
	rev     int64
	at      time.Time
	windows []codexsdk.QuotaWindow
	err     error
	errAt   time.Time
	errRev  int64
}

func readUsageCache(t *testing.T, tbl *ClientTable, accountID int64) usageCacheView {
	t.Helper()
	e := tbl.Get(accountID)
	require.NotNilf(t, e, "账号 %d 的条目应存在", accountID)
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	v := usageCacheView{
		rev:     e.usageRev,
		at:      e.usageAt,
		windows: e.usageWindows,
		err:     e.usageErr,
		errAt:   e.usageErrAt,
		errRev:  e.usageErrRev,
	}
	if e.usage != nil {
		v.hasSnap = true
		if e.usage.RateLimit != nil {
			v.percent = e.usage.RateLimit.UsedPercent
		}
	}
	return v
}

// TestUsageConcurrentSameRevisionFetchesOnce 门禁 7（设计 §8.3）/ §5.2 上界表
// "同一 (accountID,K) 并发 N 次 ⇒ 1 次上游"。
//
// 并发度取 semaphore 容量 8：**全部**请求都能同时进入拉取段（没有排队），所以
// 这条用例恰好是"双检不够"的最坏情形——双检只能防**已完成**拉取的重复，8 个
// 请求同时通过双检、一起阻塞在尚未返回的首拉上时它什么也拦不住（实测 8 次
// 上游，见报告）。真正把并发压成 1 的是 `(accountID,K)` 在飞表。
func TestUsageConcurrentSameRevisionFetchesOnce(t *testing.T) {
	const k = int64(7)
	fc := newUsageFlightCase(t, nil, 1, codexUpstreamStep{status: 200, body: usageRevBody(11)})
	u, a := fc.up, fc.a(0)
	cred := usageCredRev(1, k)

	const n = usageFetchConcurrency
	flights := make([]*usageFetch, n)
	for i := range n {
		flights[i] = startUsageFetch(a, context.Background(), cred)
	}
	u.waitArrived(t, 0) // leader 已在上游 in-flight
	u.allow(0)

	require.Equal(t, 1, u.callsN(), "同 (accountID,K) 8 并发 ⇒ 上游恰 1 次")
	require.Equal(t, 1, u.maxConcurrent(), "并发峰值 1（follower 只等结论，不打上游）")
	for i := range n {
		flights[i].wait()
		require.NoErrorf(t, flights[i].err, "第 %d 个调用必须成功", i)
		require.Same(t, flights[0].snap, flights[i].snap, "8 个调用拿到同一份观测实例")
	}
	v := readUsageCache(t, fc.tbl, 1)
	require.Equal(t, k, v.rev, "缓存标签 = 本次拉取的 K")
	require.Equal(t, 11, v.percent)
}

// TestUsageFailureCooldownPerRevision 门禁 8：失败写冷却（标上本代 K）→ 同 K
// 第二次零上游 → **推进时间**（注入时钟，不 sleep 60s）后重新拉取并成功。
func TestUsageFailureCooldownPerRevision(t *testing.T) {
	const k = int64(5)
	now := time.Date(2026, time.December, 1, 9, 0, 0, 0, time.UTC)
	fc := newUsageFlightCase(t, func() time.Time { return now }, 1,
		codexUpstreamStep{status: 500, body: `{"error":{"message":"boom"}}`},
		codexUpstreamStep{status: 200, body: usageRevBody(12)},
	)
	u, a := fc.up, fc.a(0)
	cred := usageCredRev(1, k)
	ctx := context.Background()

	u.allow(0)
	_, err := a.GetUsageSnapshot(ctx, cred)
	require.ErrorIs(t, err, ErrUpstream, "500 → ErrUpstream 分类")
	require.Equal(t, 1, u.callsN())
	v := readUsageCache(t, fc.tbl, 1)
	require.Equal(t, k, v.errRev, "失败冷却标上本次拉取的 K")
	require.Equal(t, ErrUpstream, v.err)
	require.Truef(t, v.errAt.Equal(now), "失败时刻取自注入时钟: %v", v.errAt)
	require.False(t, v.hasSnap, "失败不写快照")

	_, err = a.GetUsageSnapshot(ctx, cred)
	require.ErrorIs(t, err, ErrUpstream, "同 K 冷却内返回分类哨兵")
	require.Equal(t, 1, u.callsN(), "同 K 冷却内零上游")

	// 推进时间越过冷却（禁 sleep：时钟是注入的）。
	now = now.Add(a.usageCooldown + time.Second)
	u.allow(1)
	snap, err := a.GetUsageSnapshot(ctx, cred)
	require.NoError(t, err, "冷却到期后重新拉取")
	require.Equal(t, 12, snap.RateLimit.UsedPercent)
	require.Equal(t, 2, u.callsN())
	v = readUsageCache(t, fc.tbl, 1)
	require.Equal(t, k, v.rev, "成功写回本代标签")
	require.Truef(t, v.at.Equal(now), "成功时刻取自注入时钟: %v", v.at)
	require.True(t, v.errAt.IsZero(), "成功清冷却起点")
	require.Zero(t, v.errRev, "成功清冷却代际标签")
	require.Nil(t, v.err)
}

// TestUsageCacheIsGenerationBound 门禁 15：K=2 拉一次 → K=3 必须重新拉
// （标签翻到 3）→ 再用 K=2 仍必须重新拉。**跨代际永不命中**。
//
// 第三步同时钉住 R2-Q5 的另一半：开始**之前**就被别的代际抬升过 `srcRev`，
// 不构成"本次拉取期间身份前进"，所以不返回 ErrUsageStale——调用方拿到自己
// 这一代的观测（丢弃只针对拉取**在途**期间的抬升，门禁 27）；但缓存也不降级
// （S4a 整改 §1 单调闸门：新一代已写过 ⇒ 旧代观测只回给调用方，不落缓存）。
func TestUsageCacheIsGenerationBound(t *testing.T) {
	fc := newUsageFlightCase(t, nil, 1,
		codexUpstreamStep{status: 200, body: usageRevBody(21)},
		codexUpstreamStep{status: 200, body: usageRevBody(22)},
		codexUpstreamStep{status: 200, body: usageRevBody(23)},
	)
	u, a := fc.up, fc.a(0)
	cred2, cred3 := usageCredRev(1, 2), usageCredRev(1, 3)
	ctx := context.Background()

	u.allow(0)
	s2, err := a.GetUsageSnapshot(ctx, cred2)
	require.NoError(t, err)
	require.Equal(t, 21, s2.RateLimit.UsedPercent)
	require.Equal(t, 1, u.callsN(), "K=2 首次拉取 1 次上游")
	require.Equal(t, int64(2), readUsageCache(t, fc.tbl, 1).rev)

	u.allow(1)
	s3, err := a.GetUsageSnapshot(ctx, cred3)
	require.NoError(t, err)
	require.Equal(t, 22, s3.RateLimit.UsedPercent, "K=3 不得命中 K=2 的快照")
	require.Equal(t, 2, u.callsN(), "换代必须重新拉取")
	v := readUsageCache(t, fc.tbl, 1)
	require.Equal(t, int64(3), v.rev, "标签跟随后一次拉取")
	require.Equal(t, 22, v.percent)

	u.allow(2)
	s2b, err := a.GetUsageSnapshot(ctx, cred2)
	require.NoError(t, err, "拉取开始前的 srcRev 抬升不否掉本次观测")
	require.Equal(t, 23, s2b.RateLimit.UsedPercent, "K=2 不得命中 K=3 的快照")
	require.Equal(t, 3, u.callsN(), "无跨代际命中：K=2 再拉一次")
	// S4a 整改 §1：旧代际不降级——调用方拿到自己这一代的观测（23），但缓存仍是
	// K=3 那份（usageRev 只许单调不减）。
	v = readUsageCache(t, fc.tbl, 1)
	require.Equal(t, int64(3), v.rev, "旧代观测不得把 usageRev 降级（§1 单调闸门）")
	require.Equal(t, 22, v.percent, "新一代的事实原样保留")
}

// TestUsageOlderFetchDoesNotOverwriteFresherCache 门禁 16（设计 §5.3 新鲜度）：
// 同 K 两个拉取交错，**后返回的不得覆盖先返回的** usageAt/usageWindows。
//
// 编排：两个适配器共享同一份 ClientTable（构造期注入的共享组件，生产里同一个
// 表也交给调度器），因此共享同一条目与同一份 usage 缓存，但各有各的在飞表——
// 于是"同 K 两个上游观测同时在途"可以精确构造出来。第二个适配器的拉取**开始于**
// 第一份写入之前 ⇒ 它的观测不算更新鲜。
func TestUsageOlderFetchDoesNotOverwriteFresherCache(t *testing.T) {
	const k = int64(4)
	fc := newUsageFlightCase(t, nil, 2,
		codexUpstreamStep{status: 200, body: usageRevBody(31)},
		codexUpstreamStep{status: 200, body: usageRevBody(32)},
	)
	u, a1, a2 := fc.up, fc.a(0), fc.a(1)
	cred := usageCredRev(1, k)
	ctx := context.Background()

	f1 := startUsageFetch(a1, ctx, cred)
	u.waitArrived(t, 0) // 先发者已在上游 in-flight

	f2 := startUsageFetch(a2, ctx, cred)
	u.waitArrived(t, 1) // 后发者也已在上游（不同实例 ⇒ 不同单飞表）

	// 先发者先返回：它写下的就是"更新鲜"的那份。
	u.allow(0)
	f1.wait()
	require.NoError(t, f1.err)
	first := readUsageCache(t, fc.tbl, 1)
	require.True(t, first.hasSnap)
	require.Equal(t, 31, first.percent)
	require.Equal(t, k, first.rev)

	// 后发者晚返回：它开始于 first 写入之前 ⇒ 观测不更新鲜 ⇒ 不覆盖。
	u.allow(1)
	f2.wait()
	require.NoError(t, f2.err, "不覆盖不是错误：调用方拿到缓存里更鲜的那份")
	after := readUsageCache(t, fc.tbl, 1)
	require.True(t, first.at.Equal(after.at), "后返回者不得改写缓存时刻: %v → %v", first.at, after.at)
	require.Equal(t, first.percent, after.percent, "后返回者不得改写缓存数据")
	require.Equal(t, first.rev, after.rev)
	require.Equal(t, first.windows, after.windows, "窗口事实也不得被后返回者覆盖")
	require.Same(t, f1.snap, f2.snap, "后返回者拿到缓存里更鲜的那份（与先返回者是同一实例）")
}

// TestUsageEqualTimestampKeepsCache 门禁 16 的边界：`!usageAt.Before(fetchStartedAt)`
// 是 `>=`——两个观测拿到**逐纳秒相等**的时刻 ⇒ 无法判先后 ⇒ 保守保留缓存。
//
// 编排同上一条（共享条目的两个适配器、各有各的在飞表），但时钟**固定不动**：
// Windows 上 time.Now 的粒度可以粗到毫秒，真实交错真的会撞到相等时刻。
func TestUsageEqualTimestampKeepsCache(t *testing.T) {
	const k = int64(4)
	now := time.Date(2026, time.December, 1, 9, 0, 0, 0, time.UTC)
	fc := newUsageFlightCase(t, func() time.Time { return now }, 2,
		codexUpstreamStep{status: 200, body: usageRevBody(33)},
		codexUpstreamStep{status: 200, body: usageRevBody(34)},
	)
	u, a1, a2 := fc.up, fc.a(0), fc.a(1)
	cred := usageCredRev(1, k)
	ctx := context.Background()

	f1 := startUsageFetch(a1, ctx, cred)
	u.waitArrived(t, 0)

	f2 := startUsageFetch(a2, ctx, cred)
	u.waitArrived(t, 1)

	u.allow(0)
	f1.wait()
	require.NoError(t, f1.err)
	require.Equal(t, 33, f1.snap.RateLimit.UsedPercent)
	first := readUsageCache(t, fc.tbl, 1)
	require.Truef(t, first.at.Equal(now), "写入的时刻取自注入时钟: %v", first.at)

	// 后发者的 fetchStartedAt 与缓存时刻**相等**：判不出先后 ⇒ 保守保留缓存。
	u.allow(1)
	f2.wait()
	require.NoError(t, f2.err, "相等时刻不算错误：调用方拿到缓存里那份")
	require.Same(t, f1.snap, f2.snap, "两个观测拿到逐纳秒相等的时刻 ⇒ 保守保留缓存（同一实例）")
	after := readUsageCache(t, fc.tbl, 1)
	require.Truef(t, after.at.Equal(now), "缓存时刻不得被后返回者改写: %v", after.at)
	require.Equal(t, 33, after.percent, "缓存数据不得被后返回者覆盖")
	require.Equal(t, k, after.rev)
	require.Equal(t, 2, u.callsN(), "两个适配器各有各的在飞表 ⇒ 各打一次上游")
}

// TestUsageInterleavedRevisionsDoNotCrossHit 门禁 27：K=2 在途时 K=3 到达并
// 发起；K=2 结果先回 ⇒ 不得被当成 K=3 的事实（丢弃 + ErrUsageStale），缓存最终
// 持有 K=3 那份；两个代际的失败冷却互不阻挡。整组配合 -race 跑。
func TestUsageInterleavedRevisionsDoNotCrossHit(t *testing.T) {
	boom := `{"error":{"message":"boom"}}`
	fc := newUsageFlightCase(t, nil, 1,
		codexUpstreamStep{status: 200, body: usageRevBody(41)}, // K=2 的观测（将被丢弃）
		codexUpstreamStep{status: 200, body: usageRevBody(42)}, // K=3 的观测
		codexUpstreamStep{status: 200, body: usageRevBody(43)}, // K=2 的第二次（新拉取）
	)
	u, a := fc.up, fc.a(0)
	cred2, cred3 := usageCredRev(1, 2), usageCredRev(1, 3)
	ctx := context.Background()

	f2 := startUsageFetch(a, ctx, cred2)
	u.waitArrived(t, 0) // K=2 在上游 in-flight

	f3 := startUsageFetch(a, ctx, cred3)
	u.waitArrived(t, 1) // K=3 也发起（不同代际 ⇒ 不并到 K=2 那次拉取上）

	// K=2 结果先回：本次拉取期间身份从 2 前进到 3 ⇒ 丢弃，什么都不写。
	u.allow(0)
	f2.wait()
	require.ErrorIs(t, f2.err, ErrUsageStale, "拉取期间身份前进 ⇒ 结果丢弃")
	require.Nil(t, f2.snap)
	v := readUsageCache(t, fc.tbl, 1)
	require.False(t, v.hasSnap, "旧代际结果不得写快照")
	require.Equal(t, int64(0), v.rev, "也不得留下代际标签")
	require.Nil(t, v.windows, "丢弃不得写窗口事实")
	require.True(t, v.at.IsZero(), "丢弃不得写观测时刻")
	require.True(t, v.errAt.IsZero(), "丢弃不是失败：不得写冷却")

	// K=3 返回后才写入，标签是 3。
	u.allow(1)
	f3.wait()
	require.NoError(t, f3.err)
	require.Equal(t, 42, f3.snap.RateLimit.UsedPercent)
	v = readUsageCache(t, fc.tbl, 1)
	require.Equal(t, int64(3), v.rev, "缓存持有 K=3 那份")
	require.Equal(t, 42, v.percent)

	// K=3 再调 ⇒ 命中自己那份（零上游）。
	got3, err := a.GetUsageSnapshot(ctx, cred3)
	require.NoError(t, err)
	require.Same(t, f3.snap, got3, "同代际命中同一实例")
	require.Equal(t, 2, u.callsN())

	// K=2 再调 ⇒ 不得命中 K=3 的事实 ⇒ 重新拉（第 3 次上游）。
	u.allow(2)
	got2, err := a.GetUsageSnapshot(ctx, cred2)
	require.NoError(t, err)
	require.Equal(t, 43, got2.RateLimit.UsedPercent)
	require.Equal(t, 3, u.callsN(), "跨代际无命中")
	// S4a 整改 §1：旧代际不降级——调用方拿到自己这一代的观测（已断言 43），
	// 但缓存仍是 K=3 那一份（usageRev 只许单调不减）。
	v = readUsageCache(t, fc.tbl, 1)
	require.Equal(t, int64(3), v.rev, "旧代观测不得把 usageRev 降级（§1 单调闸门）")
	require.Equal(t, 42, v.percent, "新一代的事实原样保留")

	// 失败冷却按代际隔离：同代际内照旧生效，换代立即允许补拉。
	t.Run("failure_cooldown_is_per_revision", func(t *testing.T) {
		fc := newUsageFlightCase(t, nil, 1,
			codexUpstreamStep{status: 500, body: boom},
			codexUpstreamStep{status: 500, body: boom},
			codexUpstreamStep{status: 500, body: boom},
		)
		u, a := fc.up, fc.a(0)
		cred2, cred3 := usageCredRev(1, 2), usageCredRev(1, 3)

		u.allow(0)
		_, err := a.GetUsageSnapshot(ctx, cred2)
		require.ErrorIs(t, err, ErrUpstream)
		require.Equal(t, int64(2), readUsageCache(t, fc.tbl, 1).errRev)

		_, err = a.GetUsageSnapshot(ctx, cred2)
		require.ErrorIs(t, err, ErrUpstream)
		require.Equal(t, 1, u.callsN(), "同代际冷却生效（零上游）")

		u.allow(1)
		_, err = a.GetUsageSnapshot(ctx, cred3)
		require.ErrorIs(t, err, ErrUpstream)
		require.Equal(t, 2, u.callsN(), "K=2 的冷却不挡 K=3：换代立即补拉一次")
		require.Equal(t, int64(3), readUsageCache(t, fc.tbl, 1).errRev)

		// 反向：新代先写了失败记录 ⇒ 旧代的失败不得把它降级（S4a 整改 §1），
		// 所以旧代这次拉取照常发生（换代放行），但缓存里仍是 K=3 那段冷却。
		u.allow(2)
		_, err = a.GetUsageSnapshot(ctx, cred2)
		require.ErrorIs(t, err, ErrUpstream)
		require.Equal(t, 3, u.callsN(), "新代的冷却同样不挡旧代：换代放行一次补拉")
		require.Equal(t, int64(3), readUsageCache(t, fc.tbl, 1).errRev, "旧代的失败不降级（§1 单调闸门）")
	})
}

// usageWaitProbe 包一层 context：`Done()` 第一次被调用时关闭 `waited`——确定性
// 信号"调用方已经走到单飞的等待点"（不 sleep、不猜调度）。GetUsageSnapshot 的
// follower 路径在进入等待之前不碰 ctx（缓存/冷却/取条目都不带 ctx）。
type usageWaitProbe struct {
	context.Context
	once   sync.Once
	waited chan struct{}
}

func newUsageWaitProbe(ctx context.Context) *usageWaitProbe {
	return &usageWaitProbe{Context: ctx, waited: make(chan struct{})}
}

func (c *usageWaitProbe) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waited) })
	return c.Context.Done()
}

// TestUsageFollowerRetriesAfterLeaderCancel S4a 整改 §2：leader 的 ctx 被取消
// **不得连坐 follower**——follower 自己的调用仍然活着，它必须自己重试成新 leader。
//
// 编排：leader 卡在上游闸门 0 上（登记在飞）→ follower 进来等（probe.waited
// 表明它**确实**排队了）→ 取消 leader → follower 醒来看到的是别人的
// context.Canceled，于是有界重试（usageFollowRetries）并自己打第 2 次上游。
func TestUsageFollowerRetriesAfterLeaderCancel(t *testing.T) {
	const k = int64(6)
	fc := newUsageFlightCase(t, nil, 1,
		codexUpstreamStep{status: 200, body: usageRevBody(51)}, // leader 那次（会被取消）
		codexUpstreamStep{status: 200, body: usageRevBody(52)}, // follower 重试
	)
	u, a := fc.up, fc.a(0)
	cred := usageCredRev(1, k)

	ctx1, cancel1 := context.WithCancel(context.Background())
	leader := startUsageFetch(a, ctx1, cred)
	u.waitArrived(t, 0) // leader 已在上游 in-flight（已持有单飞登记）

	fctx := newUsageWaitProbe(context.Background())
	follower := startUsageFetch(a, fctx, cred)
	select {
	case <-fctx.waited: // follower 已排到 leader 的 flight 上（它不占上游）
	case <-time.After(5 * time.Second):
		t.Fatal("follower 未走到单飞等待点")
	}

	cancel1()
	leader.wait()
	require.ErrorIs(t, leader.err, context.Canceled, "leader 自己的取消原样返回")
	require.Nil(t, leader.snap)

	u.waitArrived(t, 1) // follower 重试 ⇒ 成为新 leader ⇒ 第 2 次上游
	u.allow(1)
	follower.wait()
	require.NoError(t, follower.err, "leader 的取消不得连坐 follower（usageFollowRetries）")
	require.Equal(t, 52, follower.snap.RateLimit.UsedPercent)
	require.Equal(t, 2, u.callsN())
	v := readUsageCache(t, fc.tbl, 1)
	require.Equal(t, k, v.rev)
	require.Equal(t, 52, v.percent)
}

// TestQuotaDeadlineAfterEntryRebuild S3a 评审要求：成功快照之后**替换该账号的
// entry**（模拟凭据重建），再调 QuotaDeadline ⇒ (zero,false,nil)——不 panic、
// 不把旧条目的 7d 窗口事实误延长到新身份上。
func TestQuotaDeadlineAfterEntryRebuild(t *testing.T) {
	const (
		week = 7 * 24 * time.Hour
		left = 6 * 24 * time.Hour
		k    = int64(3)
	)
	resetAt := time.Now().Add(left).Truncate(time.Second)
	fc := newUsageFlightCase(t, nil, 1,
		codexUpstreamStep{status: 200, body: usageWindowBody(
			100, int64(week/time.Second), resetAt.Unix(), int64(left/time.Second),
		)},
		codexUpstreamStep{status: 200, body: usageWindowBody(
			4, int64(week/time.Second), resetAt.Unix(), int64(left/time.Second),
		)},
	)
	u, a := fc.up, fc.a(0)
	cred := usageCredRev(1, k)
	ctx := context.Background()

	u.allow(0)
	until, ok, err := a.QuotaDeadline(ctx, cred)
	require.NoError(t, err)
	require.True(t, ok, "7d 窗口打满 ⇒ 有可信截止")
	require.Truef(t, until.Equal(resetAt), "截止 = %v, 期望 %v", until, resetAt)
	require.Equal(t, 1, u.callsN())
	old := a.clients.Get(1)
	require.NotNil(t, old)

	// 模拟凭据重建：管理面换 PAT ⇒ 签名变化 ⇒ 整条目替换（旧条目的 usage 事实
	// 随新条目一并消失，这正是"窗口属于旧身份那次会话"的结构保证）。
	rebuilt := &domain.AccountCredential{
		AccountID:        1,
		PATKey:           "pat-usage-rebuilt",
		IdentityRevision: k,
	}
	entry, err := a.entryFor(rebuilt)
	require.NoError(t, err)
	require.NotSame(t, old, entry, "签名变化 ⇒ 条目替换")
	fresh := readUsageCache(t, fc.tbl, 1)
	require.False(t, fresh.hasSnap, "新条目没有任何 usage 事实")
	require.Empty(t, fresh.windows)

	u.allow(1)
	until, ok, err = a.QuotaDeadline(ctx, rebuilt)
	require.NoError(t, err, "不 panic：新条目按自己的拉取给结论")
	require.False(t, ok, "窗口未打满 ⇒ 无截止（旧条目的 7d 事实不得被复用）")
	require.True(t, until.IsZero())
	require.Equal(t, 2, u.callsN(), "新条目重新拉一次（不复用旧条目缓存）")

	// 旧条目（已摘表）的观测留在它自己身上：既没被新条目偷用，也没被改写。
	old.usageMu.Lock()
	keptWindows := len(old.usageWindows)
	oldRev := old.usageRev
	old.usageMu.Unlock()
	require.Equal(t, 1, keptWindows, "旧条目的窗口事实原样保留")
	require.Equal(t, k, oldRev)
}

// TestQuotaDeadlineIgnoresForeignGenerationWindows N1：`QuotaDeadline` 读窗口事实
// 之前必须确认这份事实属于**调用方的**代际（`usageRev == cred.IdentityRevision`）。
//
// 编排（复用 `newUsageRevUpstream` 的确定性闸门）：
//
//  1. K=3 拉一次，body 是"打满且 reset 在 7 天后"的窗口 ⇒ 缓存 `usageRev=3`；
//  2. K=2 调 `QuotaDeadline`：`GetUsageSnapshot(K=2)` 因代际不符必然未命中 ⇒ 触发
//     第 2 次上游（给出未打满的 4% 窗口），而 `commitUsage` 的单调闸门
//     （`usageRev 3 > 2`）不落缓存 ⇒ 缓存仍是 K=3 那份；
//  3. 于是 K=2 必须得到 (zero, false, nil)——这条代际闸门是"不把 K=3 的 7d reset
//     当成 K=2 的冷却截止"的**唯一**防线（去掉它，这个账号会被钉死一周）；
//  4. K=3 仍读回那份 7d reset（闸门不得把本代际的事实也挡掉）。
func TestQuotaDeadlineIgnoresForeignGenerationWindows(t *testing.T) {
	const (
		week = 7 * 24 * time.Hour
		left = 6 * 24 * time.Hour
	)
	resetAt := time.Now().Add(left).Truncate(time.Second)
	fc := newUsageFlightCase(t, nil, 1,
		codexUpstreamStep{status: 200, body: usageWindowBody(
			100, int64(week/time.Second), resetAt.Unix(), int64(left/time.Second),
		)},
		codexUpstreamStep{status: 200, body: usageWindowBody(
			4, int64(week/time.Second), resetAt.Unix(), int64(left/time.Second),
		)},
	)
	u, a := fc.up, fc.a(0)
	cred3, cred2 := usageCredRev(1, 3), usageCredRev(1, 2)
	ctx := context.Background()

	u.allow(0)
	until, ok, err := a.QuotaDeadline(ctx, cred3)
	require.NoError(t, err)
	require.True(t, ok, "K=3 的 7d 打满窗口 ⇒ 有可信截止")
	require.Truef(t, until.Equal(resetAt), "截止 = %v, 期望 %v", until, resetAt)
	require.Equal(t, 1, u.callsN())
	v := readUsageCache(t, fc.tbl, 1)
	require.Equal(t, int64(3), v.rev)
	require.Len(t, v.windows, 1, "K=3 留下一份窗口事实")

	// K=2：代际不符 ⇒ 未命中 ⇒ 第 2 次上游；但单调闸门不落缓存。
	u.allow(1)
	until, ok, err = a.QuotaDeadline(ctx, cred2)
	require.NoError(t, err, "代际不符不是错误，只是没有本代际的窗口事实")
	require.False(t, ok, "不得把 K=3 的 7d reset 当成 K=2 的冷却截止")
	require.True(t, until.IsZero(), "无截止时必须是零值时刻")
	require.Equal(t, 2, u.callsN(), "代际不符必须重新拉取（不得跨代际命中）")
	v = readUsageCache(t, fc.tbl, 1)
	require.Equal(t, int64(3), v.rev, "K=2 的观测不得把 usageRev 降级（§1 单调闸门）")
	require.Len(t, v.windows, 1, "缓存里仍是 K=3 那份窗口事实")
	require.Equal(t, 100, v.percent, "K=3 打满的那份快照原样保留")

	// K=3 读回自己的事实：闸门只挡别人的代际。
	until, ok, err = a.QuotaDeadline(ctx, cred3)
	require.NoError(t, err)
	require.True(t, ok, "本代际的窗口事实不受闸门影响")
	require.Truef(t, until.Equal(resetAt), "截止 = %v, 期望 %v", until, resetAt)
	require.Equal(t, 2, u.callsN(), "K=3 命中缓存 ⇒ 零上游")
}
