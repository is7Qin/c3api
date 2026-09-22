// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// healthTestIdentity 是健康键身份分量在测试里的取值：写入与读取自洽即可，
// 不必是真实候选指纹（PROBING 记录本身按通配身份写，任何取值都能命中）。
const healthTestIdentity = "fp-test"

// --- 探测适配器测试夹具（cmd/server 局部；lookup 注入 func 源，无需调度器
// 完整夹具——ProbeAccount 权威面由 scheduler 包测试覆盖） ---

func probeTpl(id int64, cred credential.Type, formats ...domain.RequestFormat) *domain.Template {
	return &domain.Template{ID: id, BaseURL: "https://tpl.example", CredentialType: cred,
		SupportedFormats: formats}
}

// probeAcc 账号级 base_url 覆盖指到 httptest 上游（探测走真实 base 解析路径：
// 账号覆盖优先于模板——与 aiclient 取 base 同契约）。
func probeAcc(id int64, t *domain.Template, rev int64, baseURL string) *domain.Account {
	return &domain.Account{ID: id, TemplateID: t.ID, Template: t, UpstreamKey: "sk-probe",
		// 两个代际都置 rev：探测按**身份代际 K** 校验（key.IdentityRevision），
		// 而 C 是客户端 CAS 令牌。测试意图是「账号处于代际 rev」，故两者同置。
		LifecycleRevision: rev, IdentityRevision: rev, BaseURL: &baseURL}
}

// probeLookup 闭合账号表的最小 lookup 源（装配点传 sched.ProbeAccount）。
func probeLookup(accounts map[int64]*domain.Account) func(int64) (*domain.Account, bool) {
	return func(id int64) (*domain.Account, bool) {
		a, ok := accounts[id]
		return a, ok
	}
}

type fakeCodexProber struct {
	got   *domain.AccountCredential
	calls int
}

func (f *fakeCodexProber) GetUsageSnapshot(_ context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	f.calls++
	c := *cred
	f.got = &c
	return &domain.CodexUsageSnapshot{}, nil
}

func probeFn(t *testing.T, lookup func(int64) (*domain.Account, bool), codex codexUsageProber) scheduler.ProbeFunc {
	t.Helper()
	return newHealthProber(lookup, codex, 5*time.Second)
}

// TestHealthProbeRevisionFenceFailClosed：账号缺失（已删/未加载）与 revision
// 错配一律失败——stale PROBING 记录不可能经错配的 probe 变 READY。
func TestHealthProbeRevisionFenceFailClosed(t *testing.T) {
	ant := probeTpl(2, credential.TypeAPIKey, domain.FormatAnthropic)
	codex := &fakeCodexProber{}
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, ant, 5, "http://unused.invalid"),
	}), codex)

	err := fn(context.Background(), scheduler.HealthKey{AccountID: 9, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 1})
	require.ErrorIs(t, err, scheduler.ErrProbeStaleRevision,
		"missing account must fail closed")
	err = fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 4})
	require.ErrorIs(t, err, scheduler.ErrProbeStaleRevision,
		"stale revision must fail closed")
	require.Zero(t, codex.calls)
}

// TestHealthProbeAPIKeyFamilyNoNetworkNoop（owner 裁决）：api_key/responses-special
// 无合成探测面——GET /v1/models 探针已删除（端点级限流下 models-200 与
// chat-429 无关，不可靠）；探测视为通过且绝不打任何网络请求，恢复由时间窗 +
// 真实流量判定。
func TestHealthProbeAPIKeyFamilyNoNetworkNoop(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name string
		cred credential.Type
		fmt  domain.RequestFormat
	}{
		{"api_key", credential.TypeAPIKey, domain.FormatOpenAIChat},
		{"responses-special", credential.TypeResponsesSpecial, domain.FormatOpenAIResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tpl := probeTpl(1, tc.cred, tc.fmt)
			fn := probeFn(t, probeLookup(map[int64]*domain.Account{
				7: probeAcc(7, tpl, 3, upstream.URL),
			}), nil)
			require.NoError(t, fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 3}),
				"api_key 族探测视为通过（无合成探测面）")
		})
	}
	require.Zero(t, hits.Load(), "api_key 族探测不得发起任何网络请求")
}

// TestHealthProbeCodexRoutesToAdapter：codex 凭据类型（oauth/pat）探测必须走
// SDK 适配层 usage 快照路径（凭据栈权威），凭据带账号 ID。
func TestHealthProbeCodexRoutesToAdapter(t *testing.T) {
	codex := &fakeCodexProber{}

	pat := probeTpl(3, credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		11: probeAcc(11, pat, 2, "http://unused.invalid"),
	}), codex)

	require.NoError(t, fn(context.Background(), scheduler.HealthKey{AccountID: 11, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 2}))
	require.Equal(t, 1, codex.calls, "codex credential probe must route to the SDK adapter")
	require.Equal(t, int64(11), codex.got.AccountID, "probe credential must carry the account id")
}

// TestHealthProbeNilCodexAdapterFailClosed：codex 凭据但适配器未装配 → 探测
// 失败（fail-closed），不得退化为打通用 models GET。
func TestHealthProbeNilCodexAdapterFailClosed(t *testing.T) {
	pat := probeTpl(3, credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		11: probeAcc(11, pat, 2, "http://unused.invalid"),
	}), nil)

	require.Error(t, fn(context.Background(), scheduler.HealthKey{AccountID: 11, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 2}))
}

// TestHealthProbeUnknownCredTypeExplicitError：未知凭据类型显式报错——
// 不得静默 fallback 到 api_key 探测面（项目反例 3 同款纪律）。
func TestHealthProbeUnknownCredTypeExplicitError(t *testing.T) {
	weird := probeTpl(9, credential.Type("bogus"), domain.FormatOpenAIChat)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, weird, 1, "http://unused.invalid"),
	}), nil)

	require.Error(t, fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 1}))
}

// controllableCodexProber 可切换失败态的 codex 探测替身（并发安全——探测在
// RuntimeHealth 循环 goroutine 执行）。
type controllableCodexProber struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (f *controllableCodexProber) GetUsageSnapshot(_ context.Context, _ *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("probe upstream failure")
	}
	return &domain.CodexUsageSnapshot{}, nil
}
func (f *controllableCodexProber) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}
func (f *controllableCodexProber) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestHealthProbeRecoverySurface 端到端恢复面（真实适配器 + miniredis +
// Start/Close 生命周期）：codex 族可探（api_key 族已无合成探测面）——PROBING
// → 两次探测成功 → READY；探测失败 → 重开。Eventually 有界收口（probe tick
// 1s，等待上限 8s），Close 必须 join 双循环。
func TestHealthProbeRecoverySurface(t *testing.T) {
	codex := &controllableCodexProber{}
	pat := probeTpl(3, credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	c := newAssemblyRedis(t)
	h := scheduler.NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, h.Start(ctx, newHealthProber(probeLookup(map[int64]*domain.Account{
		1: probeAcc(1, pat, 5, "http://unused.invalid"),
	}), codex, time.Second)))
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		require.NoError(t, h.Close(closeCtx), "Close must join both loops")
	})

	require.NoError(t, h.SetProbing(context.Background(), 1, 5))
	// 先等探针真正跑满两次（同 gen 两次成功才 READY；也排除视图尚未同步时
	// EffectiveState 空视图默认 READY 的假阳性），再等 READY 落定。
	require.Eventually(t, func() bool {
		return codex.callCount() >= 2
	}, 8*time.Second, 200*time.Millisecond, "probe must be dispatched twice (two current-gen successes)")
	require.Eventually(t, func() bool {
		return scheduler.StateReady == h.EffectiveState(1, "*", healthTestIdentity, 5)
	}, 8*time.Second, 200*time.Millisecond, "two probe successes must reach READY via the real probe adapter")

	// 再入 PROBING 后探测持续失败：重开，不得 READY。
	codex.setFail(true)
	require.NoError(t, h.SetProbing(context.Background(), 1, 5))
	require.Eventually(t, func() bool {
		return scheduler.StateOPEN == h.EffectiveState(1, "*", healthTestIdentity, 5)
	}, 8*time.Second, 200*time.Millisecond, "probe failure must reopen the record")
}
