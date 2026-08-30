// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

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
		Status: domain.StatusActive, LifecycleRevision: rev, BaseURL: &baseURL}
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

func probeFn(t *testing.T, lookup func(int64) (*domain.Account, bool), codex codexUsageProber,
	hc *http.Client) scheduler.ProbeFunc {
	t.Helper()
	return newHealthProber(lookup, codex, hc, 5*time.Second)
}

// TestHealthProbeRevisionFenceFailClosed：账号缺失（已删/未加载）与 revision
// 错配一律失败——stale PROBING 记录不可能经错配的 probe 变 READY。
func TestHealthProbeRevisionFenceFailClosed(t *testing.T) {
	ant := probeTpl(2, credential.TypeAPIKey, domain.FormatAnthropic)
	codex := &fakeCodexProber{}
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, ant, 5, "http://unused.invalid"),
	}), codex, http.DefaultClient)

	require.Error(t, fn(context.Background(), scheduler.HealthKey{AccountID: 9, Quality: "*", Revision: 1}),
		"missing account must fail closed")
	require.Error(t, fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Revision: 4}),
		"stale revision must fail closed")
	require.Zero(t, codex.calls)
}

// TestHealthProbeAPIKeyUsesBareRootModelsGET：api_key 族探测 = GET {base}/v1/models
//（base_url 裸根契约 + openai 族补 /v1，aiclient 同款），鉴权 = Bearer；2xx 即健康。
func TestHealthProbeAPIKeyUsesBareRootModelsGET(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	chat := probeTpl(1, credential.TypeAPIKey, domain.FormatOpenAIChat)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, chat, 3, upstream.URL),
	}), nil, upstream.Client())

	require.NoError(t, fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Revision: 3}))
	require.Equal(t, http.MethodGet, gotMethod)
	require.Equal(t, "/v1/models", gotPath, "probe must hit bare-root /v1/models")
	require.Equal(t, "Bearer sk-probe", gotAuth, "openai-family probe must use Bearer auth")
}

// TestHealthProbeAnthropicUsesXAPIKey：anthropic 格式族模板探测用 x-api-key +
// anthropic-version（上游真实鉴权面），非 Bearer。
func TestHealthProbeAnthropicUsesXAPIKey(t *testing.T) {
	var gotXKey, gotVer, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXKey = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	ant := probeTpl(2, credential.TypeAPIKey, domain.FormatAnthropic)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, ant, 1, upstream.URL),
	}), nil, upstream.Client())

	require.NoError(t, fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Revision: 1}))
	require.Equal(t, "sk-probe", gotXKey, "anthropic probe must use x-api-key")
	require.NotEmpty(t, gotVer, "anthropic probe must send anthropic-version")
	require.Empty(t, gotAuth, "anthropic probe must not set Authorization")
}

// TestHealthProbeUpstreamRejectedStatusOnly：非 2xx = 探测失败（重开记录）；
// 错误只含状态码不含上游 body（防内部信息外溢——错误原文不透传契约）。
func TestHealthProbeUpstreamRejectedStatusOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("SECRET-INTERNAL-DETAIL"))
	}))
	defer upstream.Close()

	chat := probeTpl(1, credential.TypeAPIKey, domain.FormatOpenAIChat)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, chat, 1, upstream.URL),
	}), nil, upstream.Client())

	err := fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Revision: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "503", "probe error must carry the status code as verdict")
	require.NotContains(t, err.Error(), "SECRET-INTERNAL-DETAIL", "upstream body must not leak into probe errors")
}

// TestHealthProbeCodexRoutesToAdapter：codex 凭据类型（oauth/pat）探测必须走
// SDK 适配层 usage 快照路径（凭据栈权威），不打通用 models GET，凭据带账号 ID。
func TestHealthProbeCodexRoutesToAdapter(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	codex := &fakeCodexProber{}

	pat := probeTpl(3, credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		11: probeAcc(11, pat, 2, upstream.URL),
	}), codex, upstream.Client())

	require.NoError(t, fn(context.Background(), scheduler.HealthKey{AccountID: 11, Quality: "*", Revision: 2}))
	require.Equal(t, 1, codex.calls, "codex credential probe must route to the SDK adapter")
	require.Equal(t, int64(11), codex.got.AccountID, "probe credential must carry the account id")
	require.Zero(t, upstreamHits, "codex probe must not hit the generic models endpoint")
}

// TestHealthProbeNilCodexAdapterFailClosed：codex 凭据但适配器未装配 → 探测
// 失败（fail-closed），不得退化为打通用 models GET。
func TestHealthProbeNilCodexAdapterFailClosed(t *testing.T) {
	pat := probeTpl(3, credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		11: probeAcc(11, pat, 2, "http://unused.invalid"),
	}), nil, http.DefaultClient)

	require.Error(t, fn(context.Background(), scheduler.HealthKey{AccountID: 11, Quality: "*", Revision: 2}))
}

// TestHealthProbeUnknownCredTypeExplicitError：未知凭据类型显式报错——
// 不得静默 fallback 到 api_key 探测面（项目反例 3 同款纪律）。
func TestHealthProbeUnknownCredTypeExplicitError(t *testing.T) {
	weird := probeTpl(9, credential.Type("bogus"), domain.FormatOpenAIChat)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, weird, 1, "http://unused.invalid"),
	}), nil, http.DefaultClient)

	require.Error(t, fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Revision: 1}))
}

// TestHealthProbeRecoverySurface 端到端恢复面（真实适配器 + miniredis +
// Start/Close 生命周期）：PROBING → 两次上游成功 → READY；上游失败 → 重开。
// Eventually 有界收口（probe tick 1s，等待上限 8s），Close 必须 join 双循环。
func TestHealthProbeRecoverySurface(t *testing.T) {
	var upFail atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if upFail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	chat := probeTpl(1, credential.TypeAPIKey, domain.FormatOpenAIChat)
	c := newAssemblyRedis(t)
	h := scheduler.NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil, nil)
	h.SetProbeFn(newHealthProber(probeLookup(map[int64]*domain.Account{
		1: probeAcc(1, chat, 5, up.URL),
	}), nil, up.Client(), time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, h.Start(ctx))
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		require.NoError(t, h.Close(closeCtx), "Close must join both loops")
	})

	require.NoError(t, h.SetProbing(context.Background(), 1, 5))
	require.Eventually(t, func() bool {
		return scheduler.StateReady == h.EffectiveState(1, "*", 5)
	}, 8*time.Second, 200*time.Millisecond, "two upstream successes must reach READY via the real probe adapter")

	// 再入 PROBING 后上游持续 5xx：探测失败 → 重开，不得 READY。
	upFail.Store(true)
	require.NoError(t, h.SetProbing(context.Background(), 1, 5))
	require.Eventually(t, func() bool {
		return scheduler.StateOPEN == h.EffectiveState(1, "*", 5)
	}, 8*time.Second, 200*time.Millisecond, "upstream failure must reopen the record")
}
