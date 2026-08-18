// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// ---------------------------------------------------------------------------
// mock 上游：usage 端点（ChatGPT 面 wham/usage——cred.BaseURL = srv.URL +
// "/codex/responses"，SDK usageEndpointFrom 派生 /wham/usage）
// ---------------------------------------------------------------------------

// usageUpstreamCapture usage 端点 mock：可编程响应序列 + 并发计数（in-flight
// 峰值）+ 可阻塞（release 非 nil 时 handler 收包后才响应——并发节流测试用）。
type usageUpstreamCapture struct {
	mu      sync.Mutex
	calls   int
	con     int
	maxCon  int
	paths   []string
	steps   []codexUpstreamStep
	last    codexUpstreamStep
	release chan struct{}
}

func newUsageUpstream(t *testing.T, steps ...codexUpstreamStep) (*httptest.Server, *usageUpstreamCapture) {
	t.Helper()
	c := &usageUpstreamCapture{last: codexUpstreamStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.con++
		if c.con > c.maxCon {
			c.maxCon = c.con
		}
		c.paths = append(c.paths, r.URL.Path)
		step := c.last
		if len(c.steps) > 0 {
			step = c.steps[0]
			c.steps = c.steps[1:]
			c.last = step
		}
		release := c.release
		c.mu.Unlock()
		if release != nil {
			<-release
		}
		c.mu.Lock()
		c.con--
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = w.Write([]byte(step.body))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *usageUpstreamCapture) callsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *usageUpstreamCapture) maxConcurrent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxCon
}

func (c *usageUpstreamCapture) path(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[i]
}

// usageCred 构造 usage 测试凭据（PAT——零 refresh 机制，纯 usage 面）。
func usageCred(accountID int64, baseURL string) *domain.AccountCredential {
	return &domain.AccountCredential{AccountID: accountID, PATKey: "pat-usage", BaseURL: baseURL}
}

// usageOKBody 标准 usage 成功响应（全形态——含契约外 approx_*/瞬时布尔/派生
// 状态字段，收敛映射测试用）。
const usageOKBody = `{
  "plan_type": "chatgpt-plus",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {
      "used_percent": 42,
      "limit_window_seconds": 3600,
      "reset_after_seconds": 900,
      "reset_at": 1720000000
    }
  },
  "credits": {
    "has_credits": true,
    "unlimited": false,
    "overage_limit_reached": false,
    "balance": "12.50",
    "approx_local_messages": [{"k": "v"}],
    "approx_cloud_messages": [1, 2, 3]
  },
  "spend_control": {
    "reached": false,
    "individual_limit": {
      "limit": "100.00",
      "used": "30.00",
      "remaining": "70.00",
      "used_percent": 30,
      "remaining_percent": 70
    }
  },
  "rate_limit_reached_type": {"type": "rate_limit_reached", "details": "default"}
}`

// TestCodexUsageSnapshotTTL TTL 命中语义：首拉 1 次上游 → 滚动 N 次 0 次 →
// 过期重拉（时间注入——直接拨旧 e.usageAt，禁 sleep）。命中路径零分配
// （T3-4）：篡改 entry.sig 后滚动仍零上游——命中路径不计算 credSig/不重建
// （若计算签名必触发重建 → 重拉）。
func TestCodexUsageSnapshotTTL(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageOKBody})
	a := NewCodex(nil)
	cred := usageCred(1, srv.URL+"/codex/responses")
	ctx := context.Background()

	snap, err := a.GetUsageSnapshot(ctx, cred)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	require.Equal(t, 1, c.callsN(), "首拉恰 1 次上游")
	require.Equal(t, "/wham/usage", c.path(0), "端点 SDK 内部派生（ChatGPT 面 wham/usage）")

	// 命中路径零分配（T3-4）：篡改 entry.sig——命中路径若计算 credSig 比对必
	// 触发重建重拉（calls → 2）；仍恒 1 = 命中路径不做 sig 拼接/建条目。
	e, err := a.entryFor(cred)
	require.NoError(t, err)
	a.mu.Lock()
	e.sig = "corrupted-sig"
	a.mu.Unlock()

	for i := 0; i < 5; i++ {
		got, err := a.GetUsageSnapshot(ctx, cred)
		require.NoError(t, err)
		require.Same(t, snap, got, "TTL 内命中返回缓存实例（零上游）")
	}
	require.Equal(t, 1, c.callsN(), "滚动 5 次零上游（sig 被篡改仍零重建——命中路径零分配）")

	// 过期重拉（时间注入：拨旧 usageAt 越过 TTL——禁 sleep；sig 被篡改 →
	// entryFor 重建出新条目）
	e, err = a.entryFor(cred)
	require.NoError(t, err)
	a.mu.Lock()
	e.usageAt = time.Now().Add(-usageSnapshotTTL - time.Second)
	a.mu.Unlock()

	got, err := a.GetUsageSnapshot(ctx, cred)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", got.PlanType)
	require.Equal(t, 2, c.callsN(), "TTL 过期后重拉 1 次")
}

// TestCodexUsageSnapshotConcurrencyThrottle 并发节流：20 账号并发 → 上游并发
// ≤8（包级 semaphore；handler 阻塞积满并发后放行——channel 收包，禁 sleep）。
func TestCodexUsageSnapshotConcurrencyThrottle(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageOKBody})
	c.release = make(chan struct{})
	a := NewCodex(nil)
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	for i := int64(1); i <= n; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, _ = a.GetUsageSnapshot(ctx, usageCred(id, srv.URL+"/codex/responses"))
		}(i)
	}
	// 等 semaphore 饱和（8 个 in-flight 积住）——channel 信号 + 超时兜底
	deadline := time.After(5 * time.Second)
	for c.maxConcurrent() < usageFetchConcurrency {
		select {
		case <-deadline:
			t.Fatalf("usage 上游并发未达 semaphore 上限（当前 %d）", c.maxConcurrent())
		case <-time.After(time.Millisecond):
		}
	}
	close(c.release) // 放行全部 in-flight
	wg.Wait()

	require.Equal(t, usageFetchConcurrency, c.maxConcurrent(), "上游并发 ≤8（semaphore 有界）")
	require.Equal(t, n, c.callsN(), "20 账号各拉 1 次")
}

// TestCodexUsageSnapshotFailureCooldown 失败冷却（gate Major 2）：500 →
// ErrUpstream + 冷却内 N 次调用 0 次上游 → 冷却后重试成功（时间注入拨旧
// usageErrAt，禁 sleep）。
func TestCodexUsageSnapshotFailureCooldown(t *testing.T) {
	srv, c := newUsageUpstream(t,
		codexUpstreamStep{status: 500, body: `{"error":{"message":"boom"}}`},
		codexUpstreamStep{status: 200, body: usageOKBody},
	)
	a := NewCodex(nil)
	cred := usageCred(1, srv.URL+"/codex/responses")
	ctx := context.Background()

	_, err := a.GetUsageSnapshot(ctx, cred)
	require.ErrorIs(t, err, ErrUpstream, "500 → ErrUpstream 分类")
	require.Equal(t, 1, c.callsN())

	for i := 0; i < 3; i++ {
		_, err := a.GetUsageSnapshot(ctx, cred)
		require.ErrorIs(t, err, ErrUpstream, "冷却内直接返回分类错误（零上游）")
	}
	require.Equal(t, 1, c.callsN(), "冷却内 3 次调用 0 次上游")

	// 失败不缓存错误体：冷却内返回哨兵（非上游 body 错误）
	e, err := a.entryFor(cred)
	require.NoError(t, err)
	a.mu.Lock()
	require.Nil(t, e.usage, "失败不写快照缓存")
	require.Equal(t, ErrUpstream, e.usageErr, "冷却只存分类哨兵")
	// 冷却后重试（时间注入：拨旧 usageErrAt 越过冷却——禁 sleep）
	e.usageErrAt = time.Now().Add(-usageCooldown - time.Second)
	a.mu.Unlock()

	snap, err := a.GetUsageSnapshot(ctx, cred)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType, "冷却后重试 1 次成功")
	require.Equal(t, 2, c.callsN())
	// 成功清冷却态（T3-2——死状态不留）：冷却哨兵随成功归零
	e, err = a.entryFor(cred)
	require.NoError(t, err)
	a.mu.Lock()
	require.True(t, e.usageErrAt.IsZero(), "成功路径清空冷却起点")
	require.Nil(t, e.usageErr, "成功路径清空冷却哨兵")
	a.mu.Unlock()
}

// TestCodexUsageSnapshotCancelNoCooldown ctx 取消短路（task review
// 2026-08-18 Important 1）：在途拉取被取消 → 不写 60s 冷却（后续立即调用仍
// 可发起上游，不锁死账号）、返回 context.Canceled 本身（保留取消身份，不
// 误归 ErrUpstream）。首请求挂起靠 channel 闸门（禁 sleep）。
func TestCodexUsageSnapshotCancelNoCooldown(t *testing.T) {
	var (
		mu        sync.Mutex
		calls     int
		firstDone = make(chan struct{})
		release   = make(chan struct{}) // 首请求挂起闸门（测试确认取消后放行）
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(firstDone)
			<-release // 挂起直到取消后放行（channel 收包）
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(usageOKBody))
	}))
	t.Cleanup(srv.Close)
	a := NewCodex(nil)
	cred := usageCred(1, srv.URL+"/codex/responses")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-firstDone
		cancel() // 取消在途请求
	}()
	_, err := a.GetUsageSnapshot(ctx, cred)
	require.ErrorIs(t, err, context.Canceled, "取消身份保留（不误归 ErrUpstream）")
	e, err := a.entryFor(cred)
	require.NoError(t, err)
	a.mu.Lock()
	require.True(t, e.usageErrAt.IsZero(), "取消不写 60s 冷却")
	a.mu.Unlock()
	close(release) // 放行挂起的首请求（其响应随连接取消丢弃）

	snap, err := a.GetUsageSnapshot(context.Background(), cred)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	mu.Lock()
	require.Equal(t, 2, calls, "取消后立即调用仍可发起上游（未锁死）")
	mu.Unlock()
}

// TestCodexUsageSnapshotSameAccountDoubleCheck 同账号并发首拉双检（T3-3——
// 红绿用例）：N 并发同账号 → in-flight 恰 ≤8（semaphore 有界）→ 其余请求在
// 槽释放后经**二次双检**命中已完成的首拉（槽释放 ⟹ 同账号 usage 已写——写
// 先于 defer 释放槽，窗口闭合）→ 上游恰 8 次（无双检则 20 次级联全拉）+ 全
// 部返回内容一致的快照（channel 闸门同步，禁 sleep）。
//
// 注：本实现无 per-account 单飞——同账号 in-flight 突发上限 = semaphore 容量
// （8，spec「全网关 ≤8 并发」）；双检防的是**已完成**拉取的重复（队列等待者
// 零补拉），非 in-flight 去重。
func TestCodexUsageSnapshotSameAccountDoubleCheck(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageOKBody})
	c.release = make(chan struct{}) // handler 积住直至放行
	release := sync.OnceFunc(func() { close(c.release) })
	t.Cleanup(release) // 失败路径（t.Fatal 提前退出）先释放闸门——挂起 handler 不阻塞 httptest Close
	a := NewCodex(nil)
	cred := usageCred(1, srv.URL+"/codex/responses")
	ctx := context.Background()

	const n = 20
	results := make([]*domain.CodexUsageSnapshot, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = a.GetUsageSnapshot(ctx, cred)
		}(i)
	}
	// 等 semaphore 饱和（8 个 in-flight 积住）——channel 信号 + 超时兜底
	deadline := time.After(5 * time.Second)
	for c.maxConcurrent() < usageFetchConcurrency {
		select {
		case <-deadline:
			t.Fatalf("usage 上游并发未达 semaphore 上限（当前 %d）", c.maxConcurrent())
		case <-time.After(time.Millisecond):
		}
	}
	release()
	wg.Wait()

	require.Equal(t, usageFetchConcurrency, c.callsN(), "同账号 N 并发首拉 → 上游恰 8 次（in-flight 突发有界 + 双检防级联——无双检则 20 次级联全拉）")
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		// 8 个 in-flight 各自产出自有实例（最后写者胜入缓存）；等待者双检/快查
		// 命中缓存实例——内容一律相同（同一上游响应），指针可比性仅限 TTL 命
		// 中路径（TestCodexUsageSnapshotTTL 已锚定 Same）
		require.Equal(t, *results[0], *results[i], "全部返回内容一致的快照")
	}
}

// TestCodexUsageSnapshotHTTP401AuthExpired PAT 死 token 401 特判（T3-5）：SDK
// 对 PAT 不判死不轮转原样返回 HTTPError → classifyUsageErr 401 特判 →
// ErrAuthExpired（语义 = 凭据失效，非上游故障）。
func TestCodexUsageSnapshotHTTP401AuthExpired(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 401, body: `{"error":{"message":"invalid token"}}`})
	a := NewCodex(nil)
	ctx := context.Background()

	_, err := a.GetUsageSnapshot(ctx, usageCred(1, srv.URL+"/codex/responses"))
	require.ErrorIs(t, err, ErrAuthExpired, "401 → ErrAuthExpired（PAT 死 token 语义）")
	require.Equal(t, 1, c.callsN())
}

// TestCodexUsageSnapshotEntryErrAuthExpired 入口错误分类（N2）：oauth 缺 rt
// （errCredentialIncomplete——凭据不完整）→ ErrAuthExpired（不落 default 归
// ErrUpstream）。
func TestCodexUsageSnapshotEntryErrAuthExpired(t *testing.T) {
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 7, OAuthToken: "at"} // 无 rt 无 PAT
	_, err := a.GetUsageSnapshot(context.Background(), cred)
	require.ErrorIs(t, err, ErrAuthExpired, "oauth 缺 rt 凭据 → ErrAuthExpired（入口错误分类）")
}

// TestCodexUsageSnapshotFatalKeepsEntry fatal 纯判定（gate Major 3——红绿）：
// RefreshOAuth 类（FatalAuth 注入——不经 OnAuthFatal 上报面）→ ErrAuthExpired
// 且 entry 不被摘除（后续调用仍命中冷却零上游）。
func TestCodexUsageSnapshotFatalKeepsEntry(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageOKBody})
	a := NewCodex(nil)
	cred := oauthCred(1, "at-ok", "rt-ok") // oauth（rotationAuth 的 Fatal 生效；PAT Fatal 为 no-op）
	cred.BaseURL = srv.URL + "/codex/responses"
	ctx := context.Background()

	_, err := a.GetUsageSnapshot(ctx, cred)
	require.NoError(t, err)
	require.Equal(t, 1, c.callsN())

	// 凭据失效（RefreshOAuth 类）→ FatalAuth 毒化 Auth（T5——evict=false，
	// entry 保留）；GetUsageSnapshot 纯 IsFatal 判定 → ErrAuthExpired。
	// 先拨旧 usageAt（首次成功缓存仍新鲜——TTL 优先语义：≤5min 快照不被
	// 后续失败掩盖；冷却红绿断言须等 TTL 过期才可观察）。
	a.FatalAuth(1, &codexsdk.RefreshOAuthError{Code: "refresh_token_invalidated", Raw: []byte(`{}`)})
	e, err := a.entryFor(cred)
	require.NoError(t, err)
	a.mu.Lock()
	e.usageAt = time.Now().Add(-usageSnapshotTTL - time.Second)
	a.mu.Unlock()

	_, err = a.GetUsageSnapshot(ctx, cred)
	require.ErrorIs(t, err, ErrAuthExpired, "fatal → ErrAuthExpired 分类（纯判定）")
	require.Equal(t, 1, c.callsN(), "凭据失效零上游（Authorization 面直接失败）")
	a.mu.Lock()
	_, ok := a.entries[1]
	a.mu.Unlock()
	require.True(t, ok, "entry 不被摘除（GetUsageSnapshot 零副作用——红绿：translateError 路径会 evict）")

	_, err = a.GetUsageSnapshot(ctx, cred)
	require.ErrorIs(t, err, ErrAuthExpired, "fatal 后仍命中冷却（红绿断言——冷却不随 entry 消失）")
	require.Equal(t, 1, c.callsN(), "冷却内零上游")
}

// TestCodexUsageSnapshotConvergence 收敛映射（白名单）：approx_*/瞬时布尔/
// 派生状态不进契约；每块 nil → omitempty；ResetAt Unix 秒 → RFC3339；零值
// 守卫（T3-1/T3-10）：ResetAt 0 → nil 不出字段、Balance 空串 → nil 不出字段。
func TestCodexUsageSnapshotConvergence(t *testing.T) {
	srv, _ := newUsageUpstream(t,
		codexUpstreamStep{status: 200, body: usageOKBody},
		codexUpstreamStep{status: 200, body: `{"plan_type":"plan"}`},
		codexUpstreamStep{status: 200, body: `{"rate_limit":{"primary_window":{"used_percent":50}},"credits":{"balance":""}}`},
	)
	a := NewCodex(nil)
	ctx := context.Background()

	snap, err := a.GetUsageSnapshot(ctx, usageCred(1, srv.URL+"/codex/responses"))
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	require.NotNil(t, snap.RateLimit)
	require.Equal(t, 42, snap.RateLimit.UsedPercent)
	require.NotNil(t, snap.RateLimit.ResetAt)
	require.Equal(t, time.Unix(1720000000, 0).UTC(), *snap.RateLimit.ResetAt, "SDK Unix 秒 → time.Time")
	require.NotNil(t, snap.Credits)
	require.NotNil(t, snap.Credits.Balance)
	require.Equal(t, "12.50", *snap.Credits.Balance, "金额字符串不解析")
	require.NotNil(t, snap.SpendControl)
	require.Equal(t, "100.00", snap.SpendControl.Limit)
	require.Equal(t, "30.00", snap.SpendControl.Used)
	require.Equal(t, "70.00", snap.SpendControl.Remaining)
	require.Equal(t, 30, snap.SpendControl.UsedPercent)
	require.Equal(t, 70, snap.SpendControl.RemainingPercent)

	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	body := string(raw)
	for _, banned := range []string{
		"approx", "has_credits", "unlimited", "overage_limit_reached",
		"allowed", "limit_reached", "reached", "rate_limit_reached_type", "details",
	} {
		require.NotContains(t, body, banned, "契约外字段（%s）不得出现", banned)
	}
	require.Equal(t, "2024-07-03T09:46:40Z", gjson.GetBytes(raw, "rate_limit.reset_at").String(), "ResetAt RFC3339")

	// nil 块 omitempty：第二账号响应无 credits/spend_control → 不出字段
	sparse, err := a.GetUsageSnapshot(ctx, usageCred(2, srv.URL+"/codex/responses"))
	require.NoError(t, err)
	require.Nil(t, sparse.RateLimit)
	require.Nil(t, sparse.Credits)
	require.Nil(t, sparse.SpendControl)
	raw2, err := json.Marshal(sparse)
	require.NoError(t, err)
	require.Equal(t, `{"plan_type":"plan"}`, string(raw2), "nil 块 omitempty（零填充）")

	// 零值守卫：ResetAt 0（上游主窗口省略）→ RateLimit 块非 nil 但 reset_at
	// 字段不出现；Balance 空串 → credits 块整体不出（无虚假 0001-01-01/""）。
	zero, err := a.GetUsageSnapshot(ctx, usageCred(3, srv.URL+"/codex/responses"))
	require.NoError(t, err)
	require.NotNil(t, zero.RateLimit)
	require.Nil(t, zero.RateLimit.ResetAt, "ResetAt 0 → nil（不出字段）")
	require.Nil(t, zero.Credits, "Balance 空串 → credits 块 nil（不出字段）")
	raw3, err := json.Marshal(zero)
	require.NoError(t, err)
	body3 := string(raw3)
	require.Contains(t, body3, `"used_percent":50`)
	require.NotContains(t, body3, "reset_at", "零值 ResetAt 不出字段（虚假 0001-01-01 不外泄）")
	require.NotContains(t, body3, "balance", "空串 Balance 不出字段")
	require.NotContains(t, body3, "0001-01-01", "零值时间戳零填充不外泄")
}

// TestCodexUsageSnapshotEntryRebuildClears 凭据 sig 变化 + TTL 状态机：TTL
// 新鲜（命中路径零分配——T3-4）→ 快照为账号级视图，直接命中缓存（零重建零
// 重拉）；TTL 过期 + sig 变化 → entry 重建 → 快照缓存随新条目清除 → 重拉。
func TestCodexUsageSnapshotEntryRebuildClears(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageOKBody})
	a := NewCodex(nil)
	ctx := context.Background()
	base := usageCred(1, srv.URL+"/codex/responses")

	_, err := a.GetUsageSnapshot(ctx, base)
	require.NoError(t, err)
	require.Equal(t, 1, c.callsN())

	changed := usageCred(1, srv.URL+"/codex/responses")
	changed.PATKey = "pat-changed" // sig 变化
	snap, err := a.GetUsageSnapshot(ctx, changed)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	require.Equal(t, 1, c.callsN(), "TTL 内凭据变化 → 命中缓存（快照为账号级视图——零重建零重拉）")

	// TTL 过期 + sig 变化 → 重建条目（usage 随新条目清除）→ 重拉
	e, err := a.entryFor(base)
	require.NoError(t, err)
	a.mu.Lock()
	e.usageAt = time.Now().Add(-usageSnapshotTTL - time.Second)
	a.mu.Unlock()
	snap, err = a.GetUsageSnapshot(ctx, changed)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	require.Equal(t, 2, c.callsN(), "TTL 过期 + sig 变化 → 重建重拉")
}

// TestClassifyUsageErr 错误分类纯判定矩阵（gate Major 3——IsFatal/HTTPError
// 双分支零副作用）：fatal 五类 → ErrAuthExpired（含信封链穿透）；
// *HTTPError 401 → ErrAuthExpired（T3-5 PAT 死 token）；RefreshError/其余
// *HTTPError/网络 → ErrUpstream。
func TestClassifyUsageErr(t *testing.T) {
	require.ErrorIs(t, classifyUsageErr(&codexsdk.RefreshOAuthError{Code: "invalid_grant"}), ErrAuthExpired)
	require.ErrorIs(t, classifyUsageErr(&codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"}), ErrAuthExpired)
	require.ErrorIs(t, classifyUsageErr(&codexsdk.AccountDisabledError{Detail: "payment required"}), ErrAuthExpired)
	require.ErrorIs(t, classifyUsageErr(&codexsdk.CallbackDeliveryError{}), ErrAuthExpired)
	// 信封链穿透（fmt.Errorf %w 包装后仍命中）
	require.ErrorIs(t, classifyUsageErr(fmt.Errorf("codexsdk: 获取鉴权信息失败: %w", &codexsdk.RefreshOAuthError{Code: "x"})), ErrAuthExpired)

	require.ErrorIs(t, classifyUsageErr(&codexsdk.RefreshError{Attempts: 3, Err: errors.New("net")}), ErrUpstream, "RefreshError 不在 fatal 集")
	require.ErrorIs(t, classifyUsageErr(&codexsdk.HTTPError{StatusCode: 500, Raw: []byte(`{}`)}), ErrUpstream)
	require.ErrorIs(t, classifyUsageErr(&codexsdk.HTTPError{StatusCode: 401, Raw: []byte(`{}`)}), ErrAuthExpired, "401 特判（PAT 死 token）")
	require.ErrorIs(t, classifyUsageErr(&codexsdk.HTTPError{StatusCode: 403, Raw: []byte(`{}`)}), ErrUpstream, "非 401 HTTPError 仍归 ErrUpstream")
	require.ErrorIs(t, classifyUsageErr(errors.New("network error")), ErrUpstream)
}
