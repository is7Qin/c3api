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
// 过期重拉（时间注入——直接拨旧 e.usageAt，禁 sleep）。
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

	for i := 0; i < 5; i++ {
		got, err := a.GetUsageSnapshot(ctx, cred)
		require.NoError(t, err)
		require.Same(t, snap, got, "TTL 内命中返回缓存实例（零上游）")
	}
	require.Equal(t, 1, c.callsN(), "滚动 5 次零上游")

	// 过期重拉（时间注入：拨旧 usageAt 越过 TTL——禁 sleep）
	e, err := a.entryFor(cred)
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
// 派生状态不进契约；每块 nil → omitempty；ResetAt Unix 秒 → RFC3339。
func TestCodexUsageSnapshotConvergence(t *testing.T) {
	srv, _ := newUsageUpstream(t,
		codexUpstreamStep{status: 200, body: usageOKBody},
		codexUpstreamStep{status: 200, body: `{"plan_type":"plan"}`},
	)
	a := NewCodex(nil)
	ctx := context.Background()

	snap, err := a.GetUsageSnapshot(ctx, usageCred(1, srv.URL+"/codex/responses"))
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	require.NotNil(t, snap.RateLimit)
	require.Equal(t, 42, snap.RateLimit.UsedPercent)
	require.Equal(t, time.Unix(1720000000, 0).UTC(), snap.RateLimit.ResetAt, "SDK Unix 秒 → time.Time")
	require.NotNil(t, snap.Credits)
	require.Equal(t, "12.50", snap.Credits.Balance, "金额字符串不解析")
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
}

// TestCodexUsageSnapshotEntryRebuildClears 凭据 sig 变化 → entry 重建 → 快照
// 缓存一并清除 → 重拉。
func TestCodexUsageSnapshotEntryRebuildClears(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageOKBody})
	a := NewCodex(nil)
	ctx := context.Background()
	base := usageCred(1, srv.URL+"/codex/responses")

	_, err := a.GetUsageSnapshot(ctx, base)
	require.NoError(t, err)
	require.Equal(t, 1, c.callsN())

	changed := usageCred(1, srv.URL+"/codex/responses")
	changed.PATKey = "pat-changed" // sig 变化 → 重建
	snap, err := a.GetUsageSnapshot(ctx, changed)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType)
	require.Equal(t, 2, c.callsN(), "重建后快照缓存清除 → 重拉")
}

// TestClassifyUsageErr 错误分类纯判定矩阵（gate Major 3——IsFatal/HTTPError
// 双分支零副作用）：fatal 五类 → ErrAuthExpired（含信封链穿透）；RefreshError/
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
	require.ErrorIs(t, classifyUsageErr(errors.New("network error")), ErrUpstream)
}
