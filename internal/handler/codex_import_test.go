// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// —— codex 凭据批量导入 handler 契约层（400 结构校验 + 200 响应组装；行级
// 语义/幂等矩阵在 service 层与 PG 测试覆盖） ——

// codexImportTestAPI 建测试 API 并种子 codex-oauth 模板 + 分组（直接操作
// fakeStore——handler 契约层只关心请求/响应形状）。
func codexImportTestAPI(t *testing.T) (*AdminAPI, *fakeStore, int64, int64) {
	t.Helper()
	store := newFakeStore()
	tpl := &domain.Template{ID: 1, Name: "codex-tpl", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}}
	store.tpls[1] = tpl
	g := &domain.Group{ID: 3, Name: "g", Visibility: domain.GroupVisibilityPublic}
	store.groups[3] = g
	// pat 类型模板（模板类型错配 400 断言用——oauth 端点配用即拒）
	store.tpls[2] = &domain.Template{ID: 2, Name: "codex-pat-tpl", CredentialType: credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}}
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: store})
	return New(svc), store, tpl.ID, g.ID
}

func doImport(t *testing.T, h *AdminAPI, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

// doImportScoped 同 doImport 但在请求上下文注入供应商面行层作用域
// （镜像 SupplierScopeInject——导入体的 supplier_user_id 判据与归属钉死都看它）。
func doImportScoped(t *testing.T, h *AdminAPI, uid int64, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(domain.WithAccountScope(req.Context(), domain.SupplierAccountScope(uid)))
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

// TestCodexImportSupplierUserIDPresence A14②⑥（spec I4 回归）：两种导入体在
// **供应商面**出现 supplier_user_id ⇒ 400，**含显式 null**（生成类型是普通
// `*int64`，`!= nil` 判据会让 null 漏过——本用例钉住「键存在性」判据）；
// 「0」与「本人 uid」同为出现，也 400。管理面不受该判据约束（该字段在那里保留
// 但由其自身写面分配归属）。
func TestCodexImportSupplierUserIDPresence(t *testing.T) {
	endpoints := []struct {
		name string
		path string
	}{
		{"oauth", "/api/admin/accounts/batch-import-codex-oauth"},
		{"pat", "/api/admin/accounts/batch-import-codex-pat"},
	}
	// 四种出现形态：显式 null / 0 / 本人 / 他人 —— 全部 ⇒ 400。
	presence := []struct {
		name  string
		value string
	}{
		{"null", "null"},
		{"zero", "0"},
		{"self", "77"},
		{"other", "88"},
	}
	for _, ep := range endpoints {
		for _, p := range presence {
			t.Run(ep.name+" presence "+p.name+" is 400", func(t *testing.T) {
				h, _, tplID, _ := codexImportTestAPI(t)
				item := `{"codex_email":"p@example.com","codex_account_id":"p-1","codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}`
				if ep.name == "pat" {
					item = `{"codex_email":"p@example.com","codex_account_id":"p-1","codex_pat_key":"pat"}`
				}
				rec := doImportScoped(t, h, 77, ep.path,
					`{"items":[`+item+`],"template_id":`+itoa(tplID)+`,"supplier_user_id":`+p.value+`}`)
				require.Equal(t, http.StatusBadRequest, rec.Code,
					"供应商面导入体出现 supplier_user_id（含 null）必须 400：%s", rec.Body.String())
			})
		}
		// 缺席 ⇒ 放行（且归属钉死为 JWT 本人）。
		t.Run(ep.name+" absent passes and owner is jwt user", func(t *testing.T) {
			h, store, tplID, _ := codexImportTestAPI(t)
			item := `{"codex_email":"ok@example.com","codex_account_id":"ok-1","codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}`
			if ep.name == "pat" {
				item = `{"codex_email":"ok@example.com","codex_account_id":"ok-1","codex_pat_key":"pat"}`
				// pat 模板类型需匹配：oauth 模板（1）是 oauth 类型，改用 pat 模板 2。
				tplID = 2
			}
			rec := doImportScoped(t, h, 77, ep.path, `{"items":[`+item+`],"template_id":`+itoa(tplID)+`}`)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var out ImportResult
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			require.Equal(t, 1, out.Imported)
			// 归属恒为 JWT 本人（不得指向他人、不得为平台自有）。
			var found bool
			for _, a := range store.accs {
				if a.Name == "ok@example.com" {
					found = true
					require.Equal(t, int64(77), a.SupplierUserID, "导入创建的账号归属必须钉死为 JWT 本人")
				}
			}
			require.True(t, found, "账号必须已创建")
		})
	}

	// 管理面：同一字段出现**不**触发 400（该判据只在供应商面生效；归属由管理面
	// 自己的写面分配）。
	t.Run("admin surface ignores presence", func(t *testing.T) {
		h, _, tplID, _ := codexImportTestAPI(t)
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"adm@example.com","codex_account_id":"adm-1",
				"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}],
			"template_id": `+itoa(tplID)+`, "supplier_user_id": null}`)
		require.Equal(t, http.StatusOK, rec.Code, "管理面不受供应商面判据约束：%s", rec.Body.String())
	})
}

// TestPostAccountsBatchImportCodexOauthHandler oauth 端点契约：happy path
// roundtrip（imported/updated/failed 计数 + failed index=原始下标）；行级失败
// 也 200；全部失败也 200。
func TestPostAccountsBatchImportCodexOauthHandler(t *testing.T) {
	h, _, tplID, _ := codexImportTestAPI(t)

	t.Run("imported with group", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"h1@example.com","codex_account_id":"h-1",
				"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}],
			"template_id": `+itoa(tplID)+`, "group_id": 3}`)
		require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Imported)
		require.Equal(t, 0, out.Updated)
		require.Empty(t, out.Failed)
	})

	t.Run("row level failures still 200 with original index", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [
				{"codex_email":"h2@example.com","codex_account_id":"h-2",
					"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"},
				{"codex_email":"bad","codex_account_id":"h-3",
					"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"},
				{"codex_email":"h4@example.com","codex_account_id":"h-4"}
			],
			"template_id": `+itoa(tplID)+`}`)
		require.Equal(t, 200, rec.Code, "有失败行也 200: %s", rec.Body.String())
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Imported)
		require.Len(t, out.Failed, 2)
		require.Equal(t, 1, out.Failed[0].Index, "index = items 原始下标")
		require.Equal(t, 2, out.Failed[1].Index)
	})

	t.Run("all failed still 200", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"h5@example.com","codex_account_id":"h-5"}],
			"template_id": `+itoa(tplID)+`}`)
		require.Equal(t, 200, rec.Code, "全部失败也 200: %s", rec.Body.String())
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 0, out.Imported)
		require.Equal(t, 0, out.Updated)
		require.Len(t, out.Failed, 1)
		require.Equal(t, 0, out.Failed[0].Index)
	})

	t.Run("updated on re-import", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"h1@example.com","codex_account_id":"h-1",
				"codex_oauth_token":"at2","codex_oauth_refresh_token":"rt2"}],
			"template_id": `+itoa(tplID)+`}`)
		require.Equal(t, 200, rec.Code)
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 0, out.Imported)
		require.Equal(t, 1, out.Updated)
		require.Empty(t, out.Failed)
	})
}

// TestPostAccountsBatchImportCodexConfig 导入面 body 级账号配置（handler 映射面：
// upstream_cost_multiplier 正常值 → basis points 换算 + 落库；非法配置 → 400）。
func TestPostAccountsBatchImportCodexConfig(t *testing.T) {
	ctx := context.Background()
	t.Run("config mapped and persisted", func(t *testing.T) {
		h, store, tplID, _ := codexImportTestAPI(t)
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"cfg@example.com","codex_account_id":"cfg-1",
				"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}],
			"template_id": `+itoa(tplID)+`,
			"enabled": false,
			"cache_domain": "shared.example.com",
			"upstream_cost_multiplier": 2.5}`)
		require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())

		ext, err := store.FindAccountExtByCodexKey(ctx, "cfg@example.com", "cfg-1")
		require.NoError(t, err)
		acc, err := store.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.False(t, acc.Enabled, "enabled=false 落库")
		require.Equal(t, 25000, domain.MultBp(acc.UpstreamCostMultiplierBp), "2.5 → 25000 bp")
		require.NotNil(t, acc.CacheDomain)
		require.Equal(t, "shared.example.com", *acc.CacheDomain)
	})

	t.Run("invalid config rejected 400", func(t *testing.T) {
		h, store, tplID, _ := codexImportTestAPI(t)
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"bad@example.com","codex_account_id":"bad-1",
				"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}],
			"template_id": `+itoa(tplID)+`,
			"cache_domain": "Bad_Domain"}`)
		require.Equal(t, 400, rec.Code, "body: %s", rec.Body.String())
		require.Empty(t, store.accs, "整批拒绝：零落库")
	})
}

// TestPostAccountsBatchImportCodexStructural400 结构错误 → 400（items 空/超
// 100/template_id 缺/非法 json）；template_id 不存在 → 404。
func TestPostAccountsBatchImportCodexStructural400(t *testing.T) {
	h, _, tplID, _ := codexImportTestAPI(t)
	base := `{"items": [{"codex_email":"x@example.com","codex_account_id":"x",
		"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}], "template_id": `

	t.Run("empty items 400", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth",
			`{"items": [], "template_id": `+itoa(tplID)+`}`)
		require.Equal(t, 400, rec.Code)
	})

	t.Run("over 100 items 400", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString(`{"items": [`)
		for i := 0; i < 101; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"codex_email":"x` + itoa(int64(i)) + `@example.com","codex_account_id":"x` + itoa(int64(i)) + `","codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}`)
		}
		sb.WriteString(`], "template_id": ` + itoa(tplID) + `}`)
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", sb.String())
		require.Equal(t, 400, rec.Code, "items 原始条数超 100 → 400")
	})

	t.Run("template_id missing 400", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"x@example.com","codex_account_id":"x",
				"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}]}`)
		require.Equal(t, 400, rec.Code, "template_id 缺 → 400")
	})

	t.Run("template_id missing 404", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth",
			base+`999999}`)
		require.Equal(t, 404, rec.Code, "template_id 不存在 → 404: %s", rec.Body.String())
	})

	t.Run("invalid json 400", func(t *testing.T) {
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{bad`)
		require.Equal(t, 400, rec.Code)
	})

	t.Run("template type mismatch 400", func(t *testing.T) {
		// pat 类型模板（oauth 端点错误配用）→ 400 整批拒绝
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"tm@example.com","codex_account_id":"tm",
				"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}],
			"template_id": 2}`)
		require.Equal(t, 400, rec.Code, "模板类型不匹配 → 400 整批拒绝: %s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "模板类型不匹配")
	})
}

// TestPostAccountsBatchImportCodexTemplateMismatch pat 端点配 oauth 模板 →
// 400（task review Important 1——两端点对称拒绝）。
func TestPostAccountsBatchImportCodexTemplateMismatch(t *testing.T) {
	h, _, _, _ := codexImportTestAPI(t)
	rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-pat", `{
		"items": [{"codex_email":"tm2@example.com","codex_account_id":"tm2","codex_pat_key":"pat"}],
		"template_id": 1}`)
	require.Equal(t, 400, rec.Code, "pat 端点配 codex-oauth 模板 → 400: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "模板类型不匹配")
}

// TestPostAccountsBatchImportCodexPatHandler pat 端点契约（结构同 oauth；
// 含跨类型行级 failed 的响应形状）。
func TestPostAccountsBatchImportCodexPatHandler(t *testing.T) {
	h, _, tplID, _ := codexImportTestAPI(t)

	rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-pat", `{
		"items": [{"codex_email":"p1@example.com","codex_account_id":"p-1","codex_pat_key":"pat-1"}],
		"template_id": 2}`)
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	var out ImportResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 1, out.Imported)
	require.Empty(t, out.Failed)

	// oauth 端点命中 pat 行 → 行级 failed 形状（不混写）
	rec = doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
		"items": [{"codex_email":"p1@example.com","codex_account_id":"p-1",
			"codex_oauth_token":"at","codex_oauth_refresh_token":"rt"}],
		"template_id": `+itoa(tplID)+`}`)
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 0, out.Imported)
	require.Len(t, out.Failed, 1)
	require.Contains(t, out.Failed[0].Error, "凭据类型不匹配")
}
