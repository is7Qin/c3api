// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
)

type fakeSched struct{}

func (fakeSched) Runtime(id int64) (scheduler.RuntimeInfo, bool) {
	return scheduler.RuntimeInfo{Status: domain.StatusActive}, true
}
func (fakeSched) Runtimes() []scheduler.AccountRuntime { return nil }

type fakeKeys struct{ upserted, deleted []string }

func (f *fakeKeys) Upsert(hash string, meta domain.KeyMeta) {
	f.upserted = append(f.upserted, hash)
}
func (f *fakeKeys) Delete(hash string) { f.deleted = append(f.deleted, hash) }

func newTestHandler(t *testing.T) *AdminAPI {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	return New(svc)
}

func TestAdminFlow(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat","openai-responses"],"models":["gpt-4o","o3"],
		"format_models":{"openai-responses":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.True(t, tpl.FormatSupports(domain.FormatOpenAIResponses, "o3"), "format_models round-trip")
	require.False(t, tpl.FormatSupports(domain.FormatOpenAIResponses, "gpt-4o"), "responses 仅列表内模型")
	require.Equal(t, credential.TypeAPIKey, tpl.CredentialType, "缺省 credential_type → 响应含默认 api_key")

	// 非法 credential_type（未知值；合法值 codex-oauth/codex-pat 用连字符）→ 400
	recBad := do(http.MethodPost, "/admin/templates", `{
		"name":"bad","base_url":"https://u","supported_formats":["openai-chat"],
		"credential_type":"codex_oauth"}`)
	require.Equal(t, 400, recBad.Code, "非法 credential_type 必须 400: %s", recBad.Body.String())

	rec = do(http.MethodPost, "/admin/accounts", `{
		"name":"acc1","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk-x","weight":80,"max_concurrency":4}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))

	rec = do(http.MethodPost, "/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var groupResp domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groupResp))
	require.Equal(t, "g1", groupResp.Name)
	require.Equal(t, domain.GroupVisibilityPublic, groupResp.Visibility, "缺省 visibility = public")

	// 账号侧绑定分组：PUT 账号 body 带 group_ids；回显经 GET /accounts/{id}/groups 核对。
	rec = do(http.MethodPut, "/admin/accounts/"+itoa(acc.ID),
		`{"name":"acc1","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk-x","group_ids":[`+itoa(groupResp.ID)+`]}`)
	require.Equal(t, 200, rec.Code, "account-side binding: %s", rec.Body.String())
	rec = do(http.MethodGet, "/admin/accounts/"+itoa(acc.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code, "get account groups: %s", rec.Body.String())
	var accGroups AccountGroupsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accGroups))
	require.Equal(t, []int64{groupResp.ID}, accGroups.GroupIds, "账号分组回显")

	// Phase 3a：rotate-key 端点已删除（key 轮换在用户面 /user/keys/{id}/rotate）→ 404
	rec = do(http.MethodPost, "/admin/groups/"+itoa(groupResp.ID)+"/rotate-key", "")
	require.Equal(t, 404, rec.Code, "rotate-key 端点已删除: %s", rec.Body.String())

	rec = do(http.MethodGet, "/admin/stats?granularity=day", "")
	require.Equal(t, 200, rec.Code, "stats: %s", rec.Body.String())

	// 未认证 → 401
	req := httptest.NewRequest(http.MethodGet, "/admin/templates", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	require.Equal(t, 401, rec2.Code)
}

func TestAdminUpdateTemplateRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat","openai-responses"],"models":["gpt-4o","gpt-4o-mini","o3"],
		"format_models":{"openai-responses":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	// PUT 全量 snake_case body：字段必须全部生效（评审发现：原实现直接解码
	// 无 tag 的 domain.Template，base_url/supported_formats/format_models/model_mapping
	// 被丢弃 → 校验失败 400）。
	rec = do(http.MethodPut, "/admin/templates/"+itoa(tpl.ID), `{
		"name":"openai-main-v2","base_url":"https://api.openai.com/v2",
		"credential_type":"api_key",
		"supported_formats":["openai-chat","anthropic"],"models":["gpt-4o","o3"],
		"format_models":{"anthropic":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-06-01"}}`)
	require.Equal(t, 200, rec.Code, "update: %s", rec.Body.String())
	var updated domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, "openai-main-v2", updated.Name)
	require.Equal(t, credential.TypeAPIKey, updated.CredentialType, "credential_type 全量更新透传")
	require.Equal(t, "https://api.openai.com/v2", updated.BaseURL, "base_url must round-trip")
	require.ElementsMatch(t, []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic}, updated.SupportedFormats, "supported_formats must round-trip")
	require.True(t, updated.FormatSupports(domain.FormatAnthropic, "o3"), "format_models must round-trip")
	require.False(t, updated.FormatSupports(domain.FormatAnthropic, "gpt-4o"), "format_models 限制生效")
	require.Equal(t, "gpt-4o-2026-06-01", updated.ModelMapping["gpt-4o"], "model_mapping must round-trip")

	// GET 确认已持久化
	rec = do(http.MethodGet, "/admin/templates/"+itoa(tpl.ID), "")
	require.Equal(t, 200, rec.Code, "get: %s", rec.Body.String())
	var got domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, updated.BaseURL, got.BaseURL, "update must persist")
}

// 评审：参数绑定失败（InvalidParamFormatError）必须输出契约 ErrorResponse
// JSON（{"error": ...}），而非生成的 http.Error 纯文本 400。
func TestParamBindErrorIsErrorResponse(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	do := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer admin-tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		name, path string
	}{
		{"path param non-int", "/admin/templates/abc"},
		{"query limit non-int", "/admin/usage_logs?limit=abc"},
		{"query date invalid", "/admin/usage_logs?from=2026-13-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(tc.path)
			require.Equal(t, 400, rec.Code, "path %s: %s", tc.path, rec.Body.String())
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body, "error", "must be ErrorResponse JSON, got: %s", rec.Body.String())
		})
	}
}

// TestGetUsageLogs 正常路径：from/to 必填（生成层），返回 rows + next_cursor
// （空库 → next_cursor 缺省 null）。
func TestGetUsageLogs(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	req := httptest.NewRequest(http.MethodGet, "/admin/usage_logs?from=2026-08-10T00:00:00Z&to=2026-08-10T23:59:59Z", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "logs: %s", rec.Body.String())
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Nil(t, body.NextCursor, "空库无下一页")
	require.Empty(t, body.Rows)
}

// TestGetUsageLogsRequiresFromTo 无 from/to → 生成层 400（契约必填）。
func TestGetUsageLogsRequiresFromTo(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	for _, path := range []string{"/admin/usage_logs", "/admin/usage_logs?from=2026-08-10T00:00:00Z", "/admin/usage_logs?to=2026-08-10T23:59:59Z"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer admin-tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, 400, rec.Code, "path %s: %s", path, rec.Body.String())
	}
}

// TestGetErrLogsRequiresFromTo 无 from/to → 生成层 400（/err_logs 与
// /usage_logs 同契约；评审 L2：err_logs 侧缺该断言——usage 侧见
// TestGetUsageLogsRequiresFromTo）。
func TestGetErrLogsRequiresFromTo(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	for _, path := range []string{"/admin/err_logs", "/admin/err_logs?from=2026-08-10T00:00:00Z", "/admin/err_logs?to=2026-08-10T23:59:59Z"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer admin-tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, 400, rec.Code, "path %s: %s", path, rec.Body.String())
	}
}

// TestGetErrLogs /err_logs 正常路径 + 响应字段：错误审计面（status_code/
// error_type/error_message/billing_tier 全值）。
func TestGetErrLogs(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	// 空库 → 空响应（from/to 必填；next_cursor 缺省 null）
	req := httptest.NewRequest(http.MethodGet, "/admin/err_logs?from=2026-08-10T00:00:00Z&to=2026-08-10T23:59:59Z", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "err logs: %s", rec.Body.String())
	var body ErrLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Nil(t, body.NextCursor, "空库无下一页")
	require.Empty(t, body.Rows)

	// 有行 → 审计字段投影
	fs := store
	fs.mu.Lock()
	msg := "key quota exhausted"
	fs.logs = []*domain.UsageLog{
		{ID: 7, RequestID: "r-e1", UserID: 3, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, ErrorMessage: &msg, LatencyMS: 9, BillingTier: "auto",
			CreatedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)},
	}
	fs.mu.Unlock()
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "err logs: %s", rec.Body.String())
	body = ErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	e := body.Rows[0]
	require.Equal(t, int64(7), *e.ID)
	require.Equal(t, "r-e1", *e.RequestID)
	require.Equal(t, 429, *e.StatusCode, "err_logs 完整错误面 status_code")
	require.Equal(t, ErrorType("429"), *e.ErrorType)
	require.Equal(t, msg, *e.ErrorMessage)
	require.Equal(t, "auto", *e.BillingTier)
}

// TestGetLogsFilters（R4-M2/I1 评审项）日志查询过滤面真实断言——防假绿：
// 此前 fake store 仅按 user_id 过滤，model/error_type/status_code/时间/分页
// 参数恒不生效（零断言）。本测试逐参数断言 + ID 降序 + keyset 游标分页
// （cursor = 上页最后一条 id，next_cursor 由 limit+1 探测组装——与真实 repo
// QueryUsages/QueryErrLogs 语义逐项对齐）。
func TestGetLogsFilters(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)

	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: 7, KeyID: 71, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: base.Add(-2 * time.Hour)},
		{ID: 2, UserID: 7, KeyID: 72, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, CreatedAt: base.Add(-time.Hour)},
		{ID: 3, UserID: 8, KeyID: 73, Model: "o3", Format: domain.FormatOpenAIResponses,
			StatusCode: 402, ErrorType: domain.ErrBilling, CreatedAt: base},
		{ID: 4, UserID: 7, KeyID: 74, Model: "o3", Format: domain.FormatOpenAIResponses,
			StatusCode: 429, ErrorType: domain.Err429, CreatedAt: base.Add(time.Hour)},
	}
	store.mu.Unlock()
	win := "from=2026-08-10T09:00:00Z&to=2026-08-10T15:00:00Z"

	// --- /admin/usage_logs：model 过滤（精确匹配，与真实 repo ModelEQ 一致） ---
	rec := doAdmin(http.MethodGet, "/admin/usage_logs?model=gpt-4o&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "model filter: %s", rec.Body.String())
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2)
	for _, r := range body.Rows {
		require.Equal(t, "gpt-4o", *r.Model)
	}

	// error_type 过滤
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?error_type=429&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "error_type filter: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2)
	for _, r := range body.Rows {
		require.Equal(t, ErrorType("429"), *r.ErrorType)
	}

	// format 过滤（usage 侧）：openai-chat → 行 1/2（与 model 同 AND 谓词语义）
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?format=openai-chat&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "format filter: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2, "format=openai-chat → 行 1/2: %s", rec.Body.String())
	for _, r := range body.Rows {
		require.Equal(t, RequestFormat("openai-chat"), *r.Format)
	}

	// format + model 组合过滤：openai-responses + o3 → 行 3/4
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?format=openai-responses&model=o3&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "format+model filter: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2, "format=openai-responses + model=o3 → 行 3/4: %s", rec.Body.String())

	// 无效 format 值（契约不校验值域）→ 自然查空
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?format=no-such-format&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "invalid format: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body.Rows)

	// key_id 过滤（管理面 /usage_logs 新增参数；与 user_id 同 AND 谓词）
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?key_id=72&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "key filter: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "key_id=72 → 仅行 2: %s", rec.Body.String())
	require.Equal(t, int64(2), *body.Rows[0].ID)
	require.Equal(t, int64(72), *body.Rows[0].KeyID)

	// 时间范围过滤（from 含、to 含——与真实 repo CreatedAtGTE/LTE 一致）
	rec = doAdmin(http.MethodGet,
		"/admin/usage_logs?from=2026-08-10T10:30:00Z&to=2026-08-10T12:00:00Z", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "time range: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2, "时间范围 [10:30, 12:00] → 行 2/3")
	require.Equal(t, int64(3), *body.Rows[0].ID, "ID 降序不受时间过滤影响")
	require.Equal(t, int64(2), *body.Rows[1].ID)

	// keyset 游标分页：ID 降序全量 [4,3,2,1] → 首页 limit=2 → [4,3] +
	// next_cursor=3；cursor=3 → 次页 [2,1] + next_cursor 缺省（探测 ≤ limit 行）
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?limit=2&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "paging: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2)
	require.Equal(t, int64(4), *body.Rows[0].ID, "ID 降序：首页页首为 id=4")
	require.Equal(t, int64(3), *body.Rows[1].ID)
	require.NotNil(t, body.NextCursor, "探测行存在 → next_cursor = 本页最后一条 id")
	require.Equal(t, int64(3), *body.NextCursor)

	rec = doAdmin(http.MethodGet, "/admin/usage_logs?limit=2&cursor=3&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "paging: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2)
	require.Equal(t, int64(2), *body.Rows[0].ID, "cursor=3 页首为 id=2（id < 3）")
	require.Equal(t, int64(1), *body.Rows[1].ID)
	require.Nil(t, body.NextCursor, "末页 ≤ limit 行 → next_cursor 缺省")

	// 无匹配 → 空页 + next_cursor 缺省
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?model=no-such-model&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body.Rows)
	require.Nil(t, body.NextCursor)

	// limit > 200 裁剪到 200（不 400）
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?limit=500&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "limit clip: %s", rec.Body.String())
	body = LogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 4, "裁剪后仍返回全部 4 行")

	// --- /admin/err_logs：status_code 专属过滤（usage_logs 无此列） ---
	rec = doAdmin(http.MethodGet, "/admin/err_logs?status_code=429&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "status filter: %s", rec.Body.String())
	var ebody ErrLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ebody))
	require.Len(t, ebody.Rows, 2)
	for _, r := range ebody.Rows {
		require.Equal(t, 429, *r.StatusCode)
	}

	// err_logs error_type 过滤 + 响应含 status_code 全值
	rec = doAdmin(http.MethodGet, "/admin/err_logs?error_type=billing&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "err type filter: %s", rec.Body.String())
	ebody = ErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ebody))
	require.Len(t, ebody.Rows, 1)
	require.Equal(t, 402, *ebody.Rows[0].StatusCode)
	require.Equal(t, ErrorType("billing"), *ebody.Rows[0].ErrorType)
	require.Equal(t, "o3", *ebody.Rows[0].Model)

	// err_logs format 过滤：openai-chat → 行 1/2
	rec = doAdmin(http.MethodGet, "/admin/err_logs?format=openai-chat&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "err format filter: %s", rec.Body.String())
	ebody = ErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ebody))
	require.Len(t, ebody.Rows, 2, "err format=openai-chat → 行 1/2: %s", rec.Body.String())
	for _, r := range ebody.Rows {
		require.Equal(t, RequestFormat("openai-chat"), *r.Format)
	}

	// key_id 过滤（管理面 /err_logs 新增参数）
	rec = doAdmin(http.MethodGet, "/admin/err_logs?key_id=74&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "err key filter: %s", rec.Body.String())
	ebody = ErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ebody))
	require.Len(t, ebody.Rows, 1, "key_id=74 → 仅行 4: %s", rec.Body.String())
	require.Equal(t, int64(4), *ebody.Rows[0].ID)
	require.Equal(t, int64(74), *ebody.Rows[0].KeyID)
}

// TestGetUsageLogsLimitClip limit>200 裁剪真实断言（评审 L1）：上节 4 行种子
// 无法区分"裁剪到 200"与"忽略 limit"——201 行种子断言恰 200 行 + next_cursor
// 非空（裁剪后仍有下一页）。
func TestGetUsageLogsLimitClip(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)

	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store.mu.Lock()
	store.logs = make([]*domain.UsageLog, 0, 201)
	for i := 0; i < 201; i++ {
		store.logs = append(store.logs, &domain.UsageLog{
			ID: int64(i + 1), UserID: 7, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	store.mu.Unlock()
	win := "from=2026-08-10T10:00:00Z&to=2026-08-10T16:00:00Z" // 覆盖全部 201 行（12:00 起 +200min = 15:20）

	rec := doAdmin(http.MethodGet, "/admin/usage_logs?limit=500&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "limit clip: %s", rec.Body.String())
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 200, "limit=500 裁剪到 200（201 行种子区分裁剪与忽略）")
	require.Equal(t, int64(201), *body.Rows[0].ID, "首页首行 = 最新 id")
	require.Equal(t, int64(2), *body.Rows[199].ID, "第 200 条 = id 2（降序 201..2）")
	require.NotNil(t, body.NextCursor, "裁剪后仍剩 1 行 → next_cursor 非空")
	require.Equal(t, int64(2), *body.NextCursor, "next_cursor = 本页最后一条 id")
}

// newListTestRouter 列表参数测试的接线：chi + admin token 中间件 + 挂载契约路由。
func newListTestRouter(t *testing.T) (*AdminAPI, http.Handler, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	return h, r, do
}

// 列表响应从裸数组 → {total, rows} 的破坏性变更测试：全部参数绑定成功
// （fake store 不筛选，参数不报错 + 结构正确即通过）。
func TestGetTemplatesParams(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat"],"models":["gpt-4o"]}`)
	require.Equal(t, 200, rec.Code, "create: %s", rec.Body.String())

	rec = do(http.MethodGet, "/admin/templates?limit=5&offset=10&name=openai&sort=name&order=asc", "")
	require.Equal(t, 200, rec.Code, "list: %s", rec.Body.String())
	var body TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Total, "total")
	require.Len(t, body.Rows, 1, "rows")
	require.Equal(t, "openai-main", body.Rows[0].Name, "row name")
}

// status 多值（逗号分隔）+ template_id 筛选参数绑定；非法枚举值 → 400
// （openapi status 是纯 string 不校验枚举，handler 必须显式校验）。
func TestGetAccountsStatusMulti(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	rec = do(http.MethodPost, "/admin/accounts", `{
		"name":"acc1","template_id":1,"upstream_key":"sk-x","status":"active"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())

	rec = do(http.MethodGet, "/admin/accounts?status=active,disabled&template_id=1", "")
	require.Equal(t, 200, rec.Code, "list: %s", rec.Body.String())
	var body AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Total, "total")
	require.Len(t, body.Rows, 1, "rows")

	// 非法 status 枚举 → 400（handoff 硬性要求：不校验会落 repo 裸 error → 500）
	rec = do(http.MethodGet, "/admin/accounts?status=bogus", "")
	require.Equal(t, 400, rec.Code, "invalid status: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody, "error", "must be ErrorResponse JSON")
}

// 非法 sort 值 → 400（service validateListQuery 白名单校验）。
func TestGetGroupsSortInvalid(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodGet, "/admin/groups?sort=bogus", "")
	require.Equal(t, 400, rec.Code, "invalid sort: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody, "error", "must be ErrorResponse JSON")
}

// TestTemplateGroupConflict409 重复 name 创建模板/组 → 409（此前裸透传 repo 唯一
// 约束错误 → 500），且响应消息含冲突详情（name 值）。
func TestTemplateGroupConflict409(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"dup","base_url":"https://api.example.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())

	rec = do(http.MethodPost, "/admin/templates", `{
		"name":"dup","base_url":"https://api2.example.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 409, rec.Code, "重复 name 创建模板必须 409: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), `name="dup"`, "409 消息含冲突详情")

	rec = do(http.MethodPost, "/admin/groups", `{"name":"dup-g"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())

	rec = do(http.MethodPost, "/admin/groups", `{"name":"dup-g"}`)
	require.Equal(t, 409, rec.Code, "重复 name 创建分组必须 409: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), `name="dup-g"`, "409 消息含冲突详情")
}

// errMsg 解析 {"error": ...} 响应体的 error 字段（引号经 JSON 转义，需解码后断言）。
func errMsg(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	s, ok := body["error"].(string)
	require.True(t, ok, "error must be string: %s", rec.Body.String())
	return s
}

// TestSingleResourceMissingID 单资源 GET/DELETE 缺 id → 404，且响应体消息
// 含缺失 id（与批量 404 同语义；Minor T5-2 清账：handler fake 的 Get/Delete
// 需返回带 id 错误，此前仅状态码断言/缺失）。
func TestSingleResourceMissingID(t *testing.T) {
	_, _, do := newListTestRouter(t)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/templates/999"},
		{http.MethodDelete, "/admin/templates/999"},
		{http.MethodGet, "/admin/accounts/999"},
		{http.MethodDelete, "/admin/accounts/999"},
		{http.MethodGet, "/admin/groups/999"},
		{http.MethodDelete, "/admin/groups/999"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(tc.method, tc.path, "")
			require.Equal(t, 404, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
			var errBody map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
			errMsg, ok := errBody["error"].(string)
			require.True(t, ok, "error must be string: %s", rec.Body.String())
			require.Contains(t, errMsg, "id=999 missing", "404 消息含缺失 id: %s", rec.Body.String())
		})
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
