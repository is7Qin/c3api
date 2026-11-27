// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/migrate"
	"github.com/is7qin/c3api/internal/service"
)

// newLifecycleTestHandler 生命周期端点装配：直种 rev=5 已失效账号（enabled=true、
// 倍率 25000、共享域）。
func newLifecycleTestHandler(t *testing.T) (*AdminAPI, *fakeStore, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"}
	failed := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	src := "rule"
	reason := "fatal"
	dom := "shared.example.com"
	store.accs[1] = &domain.Account{
		ID: 1, Name: "acc1", TemplateID: 1, UpstreamKey: "sk-a",
		MaxConcurrency: 4, Enabled: true, FailedAt: &failed, FailureSource: &src,
		LastError: &reason, LifecycleRevision: 5, IdentityRevision: 3, UpstreamCostMultiplierBp: new(25000), CacheDomain: &dom,
	}
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: store})
	h := New(svc)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	return h, store, do
}

// TestAccountLifecycleReadContract 读面契约：GET 详情与列表视图都平铺生命周期
// 字段（Enabled/FailedAt/FailureSource/LifecycleRevision/UpstreamCostMultiplier/
// CacheDomain）；倍率边界换算 bp→正常值（25000 ↔ 2.5）。
func TestAccountLifecycleReadContract(t *testing.T) {
	_, _, do := newLifecycleTestHandler(t)

	rec := do(http.MethodGet, "/api/admin/accounts/1", "")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var acc Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.NotNil(t, acc.Enabled)
	require.True(t, *acc.Enabled)
	require.NotNil(t, acc.FailedAt, "失效时刻必须回显")
	require.NotNil(t, acc.FailureSource)
	require.Equal(t, "rule", *acc.FailureSource)
	require.NotNil(t, acc.LifecycleRevision)
	require.Equal(t, int64(5), *acc.LifecycleRevision)
	require.NotNil(t, acc.UpstreamCostMultiplier)
	require.InDelta(t, 2.5, *acc.UpstreamCostMultiplier, 1e-9, "bp 25000 → 显示 2.5")
	require.NotNil(t, acc.CacheDomain)
	require.Equal(t, "shared.example.com", *acc.CacheDomain)

	rec = do(http.MethodGet, "/api/admin/accounts", "")
	require.Equal(t, 200, rec.Code)
	var list AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.NotNil(t, list.Rows[0].LifecycleRevision, "列表视图必须平铺生命周期字段")
	require.Equal(t, int64(5), *list.Rows[0].LifecycleRevision)
	require.NotNil(t, list.Rows[0].CacheDomain)
	require.InDelta(t, 2.5, *list.Rows[0].UpstreamCostMultiplier, 1e-9)
}

// TestAccountRecoverEndpoint recover：stale revision → 409；正确 revision → 200
// 清失效三字段 + 新代际 + 清健康记录（不写任何新记录）；缺 id → 404。
func TestAccountRecoverEndpoint(t *testing.T) {
	_, store, do := newLifecycleTestHandler(t)

	rec := do(http.MethodPost, "/api/admin/accounts/1/recover", `{"expected_revision":4}`)
	require.Equal(t, 409, rec.Code, "stale revision 必须 409: %s", rec.Body.String())
	require.NotNil(t, store.accs[1].FailedAt, "409 不得清失效")

	rec = do(http.MethodPost, "/api/admin/accounts/1/recover", `{"expected_revision":5}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var acc Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.Nil(t, acc.FailedAt)
	require.Nil(t, acc.FailureSource)
	require.NotNil(t, acc.LifecycleRevision)
	require.Equal(t, int64(6), *acc.LifecycleRevision)

	require.Equal(t, 404, do(http.MethodPost, "/api/admin/accounts/999/recover", `{"expected_revision":1}`).Code)
	require.Equal(t, 400, do(http.MethodPost, "/api/admin/accounts/1/recover", `{}`).Code, "expected_revision 必填")
}

// TestAccountPatchEnabled enabled 经唯一写面：落值 + 推进 C；enable 不清失效
// 字段（恢复唯一入口 recover）。
func TestAccountPatchEnabled(t *testing.T) {
	_, store, do := newLifecycleTestHandler(t)

	rec := do(http.MethodPatch, "/api/admin/accounts/1", `{"enabled":false}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.False(t, store.accs[1].Enabled)
	require.Equal(t, int64(6), store.accs[1].LifecycleRevision, "配置写入推进 C")

	rec = do(http.MethodPatch, "/api/admin/accounts/1", `{"enabled":true}`)
	require.Equal(t, 200, rec.Code)
	require.True(t, store.accs[1].Enabled)
	require.NotNil(t, store.accs[1].FailedAt, "enable 不清失效字段（恢复唯一入口 recover）")
}

// TestAccountPatchCostMultiplier 倍率经唯一写面：正常值 → bp 换算落库；越界 → 400。
func TestAccountPatchCostMultiplier(t *testing.T) {
	_, store, do := newLifecycleTestHandler(t)

	rec := do(http.MethodPatch, "/api/admin/accounts/1", `{"upstream_cost_multiplier":1.5}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 15000, domain.MultBp(store.accs[1].UpstreamCostMultiplierBp), "1.5 → 15000bp")
	var acc Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.InDelta(t, 1.5, *acc.UpstreamCostMultiplier, 1e-9)

	require.Equal(t, 400, do(http.MethodPatch, "/api/admin/accounts/1", `{"upstream_cost_multiplier":11}`).Code, "上界 ×10")
	require.Equal(t, 400, do(http.MethodPatch, "/api/admin/accounts/1", `{"upstream_cost_multiplier":-0.5}`).Code, "下界 0")
}

// TestAccountPatchCacheDomain 缓存域经唯一写面：设置；null = 清空（回私有域）；
// "" → 400（哨兵已取消）；非法域 → 400。
func TestAccountPatchCacheDomain(t *testing.T) {
	_, store, do := newLifecycleTestHandler(t)

	rec := do(http.MethodPatch, "/api/admin/accounts/1", `{"cache_domain":"other.example.com"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "other.example.com", *store.accs[1].CacheDomain)

	rec = do(http.MethodPatch, "/api/admin/accounts/1", `{"cache_domain":""}`)
	require.Equal(t, 400, rec.Code, "空串哨兵已取消 → 400: %s", rec.Body.String())

	rec = do(http.MethodPatch, "/api/admin/accounts/1", `{"cache_domain":null,"name":"acc1"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Nil(t, store.accs[1].CacheDomain, "null = 清空回私有域")

	require.Equal(t, 400, do(http.MethodPatch, "/api/admin/accounts/1", `{"cache_domain":"BAD domain!"}`).Code)
}

// TestAccountPatchRejectsEmptyBody 空补丁（无任何字段）→ 400：写面不接受无字段
// 请求（否则等于一次纯 C 推进的空写）。
func TestAccountPatchRejectsEmptyBody(t *testing.T) {
	_, _, do := newLifecycleTestHandler(t)
	require.Equal(t, 400, do(http.MethodPatch, "/api/admin/accounts/1", `{}`).Code)
}

// TestAccountCreateCacheDomain 创建带 cache_domain → 回显；非法域 → 400。
func TestAccountCreateCacheDomain(t *testing.T) {
	_, _, do := newLifecycleTestHandler(t)

	rec := do(http.MethodPost, "/api/admin/accounts", `{"name":"acc2","template_id":1,"upstream_key":"sk-b","cache_domain":"pool.example.com"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var acc Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.NotNil(t, acc.CacheDomain)
	require.Equal(t, "pool.example.com", *acc.CacheDomain)

	require.Equal(t, 400, do(http.MethodPost, "/api/admin/accounts", `{"name":"acc3","template_id":1,"upstream_key":"sk-c","cache_domain":"BAD!"}`).Code)
}

// TestAccountPatchLeavesUnmentionedFieldsUntouched 补丁**未提及**的字段必须保持
// 原值（三态：缺省 = 不变）——enabled/倍率/缓存域不在补丁里就一个都不动。
func TestAccountPatchLeavesUnmentionedFieldsUntouched(t *testing.T) {
	_, store, do := newLifecycleTestHandler(t)
	store.accs[1].FailedAt = nil

	rec := do(http.MethodPatch, "/api/admin/accounts/1", `{"name":"renamed"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "renamed", store.accs[1].Name)
	require.True(t, store.accs[1].Enabled, "未提及 enabled → 保持")
	require.Equal(t, 25000, domain.MultBp(store.accs[1].UpstreamCostMultiplierBp), "未提及倍率 → 保持")
	require.NotNil(t, store.accs[1].CacheDomain)
	require.Equal(t, "shared.example.com", *store.accs[1].CacheDomain, "未提及缓存域 → 保持")
}

// TestRuleTypedActionContract typed Throttle/FailAccount wire 往返：open/retry_after
// 合法组合回显；互斥违规 → 400。
func TestRuleTypedActionContract(t *testing.T) {
	_, _, do := newLifecycleTestHandler(t)

	rec := do(http.MethodPost, "/api/admin/rules", `{"name":"r-open","priority":1,"when":{"kind":"429"},"then":{"throttle":{"scope":"account","mode":"open","duration_ms":60000,"use_reset":false}}}`)
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var created Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	th, ok := created.Then["throttle"]
	require.True(t, ok, "then 必须回显 throttle")
	thMap, ok := th.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "open", thMap["mode"])
	require.Equal(t, float64(60000), thMap["duration_ms"])

	rec = do(http.MethodPost, "/api/admin/rules", `{"name":"r-fail","priority":2,"when":{"kind":"network"},"then":{"fail_account":true}}`)
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var failed Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &failed))
	require.Equal(t, true, failed.Then["fail_account"])

	// 互斥：throttle + fail_account → 400；throttle + 未知键 → 400（严格边界）
	require.Equal(t, 400, do(http.MethodPost, "/api/admin/rules", `{"name":"r-x1","priority":3,"then":{"throttle":{"scope":"account","mode":"open","duration_ms":1000,"use_reset":false},"fail_account":true}}`).Code)
	require.Equal(t, 400, do(http.MethodPost, "/api/admin/rules", `{"name":"r-x2","priority":4,"then":{"throttle":{"scope":"account","mode":"retry_after","use_reset":true},"not_a_rule_field":"x"}}`).Code)
	// retry_after 必须 use_reset=true
	require.Equal(t, 400, do(http.MethodPost, "/api/admin/rules", `{"name":"r-x3","priority":5,"then":{"throttle":{"scope":"account","mode":"retry_after","use_reset":false}}}`).Code)
}

// TestAccountFreshSchemaColumns 钉死账号持久列集（fresh-only 契约）：以生成
// schema 结构（migrate.AccountsColumns）对照允许列全集——任何列的增删都会
// 先在这里失败，而不是散落在各消费方的隐式假设里。
func TestAccountFreshSchemaColumns(t *testing.T) {
	got := make([]string, 0, len(migrate.AccountsColumns))
	for _, c := range migrate.AccountsColumns {
		got = append(got, c.Name)
	}
	require.ElementsMatch(t, []string{
		"id", "name", "template_id", "base_url", "upstream_key",
		"max_concurrency", "last_error", "last_used_at", "failed_at",
		"failure_source", "enabled", "lifecycle_revision", "identity_revision",
		"upstream_cost_multiplier_bp", "cache_domain", "supplier_user_id",
		"proxy_id", // 出口代理绑定（spec proxy-pool §3.2）：NULL = 直连
		"updated_at", "deleted_at", "created_at",
	}, got, "账号列集必须与 fresh 契约允许全集一致")
}
