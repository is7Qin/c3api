// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/service"
)

// newSharedRouters 共享 fakeStore 的 admin + user 双路由（Task 4 端到端：
// 管理面建组/授予 → 用户面选组建 key），返回 doAdmin/doUser。
func newSharedRouters(t *testing.T) (doAdmin, doUser func(method, path, body, token string) *httptest.ResponseRecorder, store *fakeStore) {
	t.Helper()
	store = newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)

	// admin 路由（静态 token 中间件，模拟 server 层 /admin 鉴权）
	adminH := New(svc)
	ar := chi.NewRouter()
	ar.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	ar.Mount("/", adminH.Router())

	// user 路由（真实 Router：公开/RequireJWT 分流）
	iss := auth.NewIssuer("test-secret")
	ur := userapi.Router(svc, iss, fakeUserStatus{store: store}, nil)

	doAdmin = func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set("Authorization", "Bearer admin-tok")
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ar.ServeHTTP(rec, req)
		return rec
	}
	doUser = func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ur.ServeHTTP(rec, req)
		return rec
	}
	return doAdmin, doUser, store
}

// registerAndGet 注册并返回 JWT + 用户（handler 测试 helper）。
func registerAndGet(t *testing.T, doUser func(method, path, body, token string) *httptest.ResponseRecorder, email string) (string, int64) {
	t.Helper()
	rec := doUser(http.MethodPost, "/user/auth/register",
		`{"email":"`+email+`","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Token, *resp.User.ID
}

// TestUserKeysLifecycle 用户面 key 全生命周期 + 组可选性校验：
// public 可建 → private 未授予 400 → 管理面授予后可建 → 更新/轮换/删除 →
// 他人 key 不可见（404）。
func TestUserKeysLifecycle(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// 管理面建组：public + private
	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"public-g"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create public group: %s", rec.Body.String())
	var pubG Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pubG))
	require.Equal(t, GroupVisibility("public"), *pubG.Visibility, "缺省 visibility = public")

	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"private-g","visibility":"private"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create private group: %s", rec.Body.String())
	var privG Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &privG))
	require.Equal(t, GroupVisibility("private"), *privG.Visibility)

	token, userID := registerAndGet(t, doUser, "alice@example.com")

	// /user/groups：只看到 public（private 未授予）
	rec = doUser(http.MethodGet, "/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "groups: %s", rec.Body.String())
	var groups []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 1, "private 未授予不可见: %s", rec.Body.String())
	require.Equal(t, *pubG.ID, *groups[0].ID)

	// public 组建 key → 200（raw 明文长期回显）
	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k1","group_id":`+itoa(*pubG.ID)+`}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "create key public: %s", rec.Body.String())
	var created userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.Key, "raw 明文必须返回")
	require.True(t, strings.HasPrefix(*created.Key, "ck-"), "key 前缀约定")
	require.Equal(t, int64(0), *created.Quota, "缺省 quota = 0 不限")

	// private 组未授予 → 400（组可选性校验）
	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k2","group_id":`+itoa(*privG.ID)+`}`, token)
	require.Equal(t, http.StatusBadRequest, rec.Code, "private not granted: %s", rec.Body.String())

	// 管理面授予 → 可建
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*privG.ID)+"/assignments",
		`{"user_ids":[`+itoa(userID)+`]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "assign: %s", rec.Body.String())
	var assigned GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &assigned))
	require.Equal(t, []int64{userID}, assigned.UserIds)

	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k2","group_id":`+itoa(*privG.ID)+`}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "create key granted private: %s", rec.Body.String())
	var k2 userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &k2))

	// 列表：2 个；行含明文（长期回显契约）
	rec = doUser(http.MethodGet, "/user/keys", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "list keys: %s", rec.Body.String())
	var list userapi.KeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	require.Len(t, list.Rows, 2)
	for _, r := range list.Rows {
		require.NotEmpty(t, r.Key, "列表行含明文（长期可查看/复制）")
	}
	// 行序 = 实现细节（fake 无序 / 真实 id desc），按 ID 定位 created 行再断言
	found := false
	for i := range list.Rows {
		if *list.Rows[i].ID == *created.ID {
			found = true
			require.Equal(t, created.Key, list.Rows[i].Key, "列表明文与创建返回一致")
		}
	}
	require.True(t, found, "created 行在列表中")

	// 更新：max_concurrency/status
	rec = doUser(http.MethodPut, "/user/keys/"+itoa(*created.ID),
		`{"max_concurrency":5,"status":"disabled"}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "update key: %s", rec.Body.String())
	var updated userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, 5, *updated.MaxConcurrency)
	require.Equal(t, userapi.KeyStatus("disabled"), *updated.Status)

	// 轮换：新明文 ≠ 旧明文；prefix 变化
	rec = doUser(http.MethodPost, "/user/keys/"+itoa(*created.ID)+"/rotate", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "rotate key: %s", rec.Body.String())
	var rotated userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rotated))
	require.NotEqual(t, created.Key, rotated.Key, "轮换后明文必须变化")

	// 删除 → 列表剩 1
	rec = doUser(http.MethodDelete, "/user/keys/"+itoa(*created.ID), "", token)
	require.Equal(t, http.StatusOK, rec.Code, "delete key: %s", rec.Body.String())
	rec = doUser(http.MethodGet, "/user/keys", "", token)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)

	// 软删 key 不可复活（F2 契约）：删除后 PUT/Rotate → 404
	rec = doUser(http.MethodPut, "/user/keys/"+itoa(*created.ID), `{"name":"revived"}`, token)
	require.Equal(t, http.StatusNotFound, rec.Code, "已删 key PUT → 404: %s", rec.Body.String())
	rec = doUser(http.MethodPost, "/user/keys/"+itoa(*created.ID)+"/rotate", "", token)
	require.Equal(t, http.StatusNotFound, rec.Code, "已删 key rotate → 404: %s", rec.Body.String())

	// 他人 key 不可见：bob 读 alice 的 key → 404（防越权探测）
	bobToken, _ := registerAndGet(t, doUser, "bob@example.com")
	rec = doUser(http.MethodGet, "/user/keys/"+itoa(*k2.ID), "", bobToken)
	require.Equal(t, http.StatusNotFound, rec.Code, "cross-user key: %s", rec.Body.String())
	rec = doUser(http.MethodDelete, "/user/keys/"+itoa(*k2.ID), "", bobToken)
	require.Equal(t, http.StatusNotFound, rec.Code, "cross-user delete: %s", rec.Body.String())

	// 不存在 key → 404
	rec = doUser(http.MethodGet, "/user/keys/99999", "", token)
	require.Equal(t, http.StatusNotFound, rec.Code, "missing key: %s", rec.Body.String())

	// 未认证访问业务端点 → 401（RequireJWT）
	rec = doUser(http.MethodGet, "/user/keys", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "no token: %s", rec.Body.String())
	rec = doUser(http.MethodGet, "/user/groups", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestUserUsageLogsOwnOnly /user/usage_logs 强制 user_id = 当前用户（越权过滤在
// service/repo 层；fake store 按 user_id 过滤——测试验证隔离语义）。
func TestUserUsageLogsOwnOnly(t *testing.T) {
	_, doUser, store := newSharedRouters(t)
	tokenA, userA := registerAndGet(t, doUser, "a@example.com")
	_, userB := registerAndGet(t, doUser, "b@example.com")

	// 直接向 store 灌入两个用户的日志（含 key_id——key_id 过滤传递断言用）
	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: userA, KeyID: 11, RequestID: "r-a1", Model: "gpt-4o", Format: domain.FormatOpenAIChat, CreatedAt: base},
		{ID: 2, UserID: userB, RequestID: "r-b1", Model: "gpt-4o", Format: domain.FormatOpenAIChat, CreatedAt: base},
		{ID: 3, UserID: userA, KeyID: 33, RequestID: "r-a2", Model: "o3", Format: domain.FormatOpenAIResponses, CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	// 无 from/to → 生成层 400（user 侧契约与 admin 同）
	rec := doUser(http.MethodGet, "/user/usage_logs", "", tokenA)
	require.Equal(t, http.StatusBadRequest, rec.Code, "missing from/to: %s", rec.Body.String())

	rec = doUser(http.MethodGet, "/user/usage_logs?"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user logs: %s", rec.Body.String())
	var body userapi.UserLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2, "只看到自己的日志: %s", rec.Body.String())
	for _, r := range body.Rows {
		require.Equal(t, userA, *r.UserID, "日志必须归属当前用户")
	}
	// 用户面响应无上游拓扑字段（UserUsageLog 契约：AccountID/TemplateID 已删）
	require.NotContains(t, rec.Body.String(), "AccountID", "用户面响应不得含 AccountID")
	require.NotContains(t, rec.Body.String(), "TemplateID", "用户面响应不得含 TemplateID")

	// key_id 过滤（自己的 key）：key_id=33 → 仅行 3（user_id + key_id 双谓词）
	rec = doUser(http.MethodGet, "/user/usage_logs?key_id=33&"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "key filter: %s", rec.Body.String())
	body = userapi.UserLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "key_id=33 仅本人 key 33 的行: %s", rec.Body.String())
	require.Equal(t, int64(3), *body.Rows[0].ID)
	require.Equal(t, int64(33), *body.Rows[0].KeyID)
	require.Equal(t, userA, *body.Rows[0].UserID, "他人 key_id 探测仍被 user_id 钳制")

	// format 过滤（自己的行内筛选）：openai-responses → 仅行 3（user_id + format 双谓词）
	rec = doUser(http.MethodGet, "/user/usage_logs?format=openai-responses&"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user format filter: %s", rec.Body.String())
	body = userapi.UserLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "format=openai-responses 仅本人行 3: %s", rec.Body.String())
	require.Equal(t, int64(3), *body.Rows[0].ID)
	require.Equal(t, userapi.RequestFormat("openai-responses"), *body.Rows[0].Format)
	require.Equal(t, userA, *body.Rows[0].UserID, "越权 format 探测仍被 user_id 钳制（他人行 2 同 format 不混入）")

	// 跨页 id 注入尝试（评审 L4）：cursor=3 的谓词窗 id<3 含他人行 2——若
	// user_id 过滤缺失本页会出现 B 行（行 2 先于行 1），越权钳制在 user_id
	// 过滤不在 cursor 值（cursor=2 会把 B 行自身排除，断言无法区分）。
	rec = doUser(http.MethodGet, "/user/usage_logs?limit=1&cursor=3&"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "cursor injection: %s", rec.Body.String())
	body = userapi.UserLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "注入他人 id 游标仅本人行: %s", rec.Body.String())
	require.Equal(t, int64(1), *body.Rows[0].ID, "cursor=3（谓词窗含 B 行 2）→ 本人行 id=1")
	require.Equal(t, userA, *body.Rows[0].UserID, "行归属恒为当前用户")
	require.Nil(t, body.NextCursor, "仅 1 行 → 无下一页")
}

// TestUserErrLogsOwnOnly /user/err_logs 强制 user_id = 当前用户（与
// /user/usage_logs 同构的越权隔离）。
func TestUserErrLogsOwnOnly(t *testing.T) {
	_, doUser, store := newSharedRouters(t)
	tokenA, userA := registerAndGet(t, doUser, "a@example.com")
	_, userB := registerAndGet(t, doUser, "b@example.com")

	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	msg := "no available account"
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: userA, KeyID: 11, RequestID: "e-a1", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, ErrorMessage: &msg, CreatedAt: base},
		{ID: 2, UserID: userB, RequestID: "e-b1", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 402, ErrorType: domain.ErrBilling, CreatedAt: base},
		{ID: 3, UserID: userA, KeyID: 33, RequestID: "e-a2", Model: "o3", Format: domain.FormatOpenAIResponses,
			StatusCode: 401, ErrorType: domain.ErrAuth, CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	// 无 from/to → 生成层 400（user 侧 err_logs 契约同 usage_logs；评审 L2：
	// 此前 err_logs 双侧缺该断言，usage 侧见本文件 TestUserUsageLogsOwnOnly）
	rec := doUser(http.MethodGet, "/user/err_logs", "", tokenA)
	require.Equal(t, http.StatusBadRequest, rec.Code, "missing from/to: %s", rec.Body.String())

	rec = doUser(http.MethodGet, "/user/err_logs?"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user err logs: %s", rec.Body.String())
	var body userapi.UserErrLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2, "只看到自己的错误明细: %s", rec.Body.String())
	for _, r := range body.Rows {
		require.Equal(t, userA, *r.UserID, "错误明细必须归属当前用户")
		require.NotNil(t, r.StatusCode, "err_logs 完整错误面含 status_code")
	}
	// 用户面响应无上游拓扑字段（UserErrLog 契约：AccountID/TemplateID 已删）
	require.NotContains(t, rec.Body.String(), "AccountID", "用户面响应不得含 AccountID")
	require.NotContains(t, rec.Body.String(), "TemplateID", "用户面响应不得含 TemplateID")

	// key_id 过滤（自己的 key）：key_id=33 → 仅行 3（user_id + key_id 双谓词）
	rec = doUser(http.MethodGet, "/user/err_logs?key_id=33&"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "key filter: %s", rec.Body.String())
	body = userapi.UserErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "key_id=33 仅本人 key 33 的行: %s", rec.Body.String())
	require.Equal(t, int64(3), *body.Rows[0].ID)
	require.Equal(t, int64(33), *body.Rows[0].KeyID)
	require.Equal(t, userA, *body.Rows[0].UserID, "他人 key_id 探测仍被 user_id 钳制")

	// format 过滤（自己的行内筛选）：openai-chat → 仅行 1（他人行 2 同 format 被
	// user_id 钳制；userA 行 3 为 openai-responses 不混入）
	rec = doUser(http.MethodGet, "/user/err_logs?format=openai-chat&"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user err format filter: %s", rec.Body.String())
	body = userapi.UserErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "format=openai-chat 仅本人行 1: %s", rec.Body.String())
	require.Equal(t, int64(1), *body.Rows[0].ID)
	require.Equal(t, userapi.RequestFormat("openai-chat"), *body.Rows[0].Format)
	require.Equal(t, userA, *body.Rows[0].UserID)

	// 跨页 id 注入尝试（评审 L4）：cursor=3 的谓词窗 id<3 含他人行 2——若
	// user_id 过滤缺失本页会出现 B 行（行 2 先于行 1）。
	rec = doUser(http.MethodGet, "/user/err_logs?limit=1&cursor=3&"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "cursor injection: %s", rec.Body.String())
	body = userapi.UserErrLogsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1, "注入他人 id 游标仅本人行: %s", rec.Body.String())
	require.Equal(t, int64(1), *body.Rows[0].ID, "cursor=3（谓词窗含 B 行 2）→ 本人行 id=1")
	require.Equal(t, userA, *body.Rows[0].UserID, "行归属恒为当前用户")
	require.Nil(t, body.NextCursor, "仅 1 行 → 无下一页")
}

// TestAdminUsers 管理面用户 CRUD：创建（email 唯一/格式/密码长度）→
// 列表 → 更新（role/status/max_concurrency）→ 变更经 fakeUserStatus
// 快照即时可见。
func TestAdminUsers(t *testing.T) {
	doAdmin, _, _ := newSharedRouters(t)

	// 创建用户（缺省 role=user/status=active）
	rec := doAdmin(http.MethodPost, "/admin/users",
		`{"email":"carol@example.com","password":"s3cret-pass","max_concurrency":3}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create user: %s", rec.Body.String())
	var created User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, "carol@example.com", *created.Email)
	require.Equal(t, UserRole("user"), *created.Role)
	require.Equal(t, UserStatus("active"), *created.Status)
	require.Equal(t, 3, *created.MaxConcurrency)
	require.NotContains(t, rec.Body.String(), "password", "口令散列永不下发")

	// email 重复 → 409
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"carol@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusConflict, rec.Code, "dup email: %s", rec.Body.String())

	// 邮箱格式非法 → 400
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"not-an-email","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad email: %s", rec.Body.String())

	// 密码超长（>72 字节）→ 400
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"long@example.com","password":"`+strings.Repeat("a", 73)+`"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "long password: %s", rec.Body.String())

	// 列表 + email 筛选
	rec = doAdmin(http.MethodGet, "/admin/users?email=carol", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "list users: %s", rec.Body.String())
	var list UserListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)

	// 更新：升级 platform_admin + 禁用
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID),
		`{"role":"platform_admin","status":"disabled","max_concurrency":0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "update user: %s", rec.Body.String())
	var updated User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, UserRole("platform_admin"), *updated.Role)
	require.Equal(t, UserStatus("disabled"), *updated.Status)

	// 非法 role → 400
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"role":"bogus"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad role: %s", rec.Body.String())

	// 缺失用户 → 404
	rec = doAdmin(http.MethodPut, "/admin/users/99999", `{"role":"user"}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing user: %s", rec.Body.String())
}

// TestAdminPutUsersPatchSemantics A-P1-1 patch 形态端到端：只改 balance 的 PUT
// 不误拒（评审 P3-B 零值面）、不触碰 role/status/并发；只改 role 不触碰
// balance——GET 快照陈旧值不再全量写回（v02 核实双向覆盖修复）。
func TestAdminPutUsersPatchSemantics(t *testing.T) {
	doAdmin, doUser, store := newSharedRouters(t)
	_, uid := registerAndGet(t, doUser, "patchb@example.com")
	require.NoError(t, store.UpdateUserBalance(t.Context(), uid, 100000)) // 1 USD = 100,000 毫分

	// 只改 balance（USD 50）：Role/Status 零值不误拒；role 不被快照写回
	rec := doAdmin(http.MethodPut, "/admin/users/"+itoa(uid), `{"balance":50}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "只改 balance 不误拒: %s", rec.Body.String())
	var updated User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, 50.0, *updated.Balance, "回显 USD 换算")
	// 该用户是共享 store 的首个注册（bootstrap：空表首个 = platform_admin）；
	// 断言意图 = role 未被 GET 快照写回（保持注册时原值，平台管理员在用户前创建）
	require.Equal(t, UserRole("platform_admin"), *updated.Role, "role 未被 GET 快照写回")
	u2, err := store.GetUser(t.Context(), uid)
	require.NoError(t, err)
	require.Equal(t, int64(5000000), u2.Balance, "50 USD = 5,000,000 毫分落库")

	// 只改 role：balance 不丢
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid), `{"role":"platform_admin"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "只改 role: %s", rec.Body.String())
	u3, err := store.GetUser(t.Context(), uid)
	require.NoError(t, err)
	require.Equal(t, int64(5000000), u3.Balance, "只改 role 不触碰 balance（旧实现快照写回面）")
	require.Equal(t, domain.RolePlatformAdmin, u3.Role)

	// 空 body → 200 无副作用（全 nil patch no-op）
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid), `{}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "空 patch: %s", rec.Body.String())
	u4, err := store.GetUser(t.Context(), uid)
	require.NoError(t, err)
	require.Equal(t, int64(5000000), u4.Balance)
	require.Equal(t, domain.RolePlatformAdmin, u4.Role)
}

// TestAdminGroupAssignments 替换语义：授予/撤销/清空；用户缺失 → 404；
// 重复/非法 id → 400。
func TestAdminGroupAssignments(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"g","visibility":"private"}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))

	token1, uid1 := registerAndGet(t, doUser, "u1@example.com")
	token2, uid2 := registerAndGet(t, doUser, "u2@example.com")

	// 授予两人
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments",
		`{"user_ids":[`+itoa(uid1)+`,`+itoa(uid2)+`]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "assign two: %s", rec.Body.String())
	var resp GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.ElementsMatch(t, []int64{uid1, uid2}, resp.UserIds)

	// 两人都可见 private 组
	for _, token := range []string{token1, token2} {
		rec = doUser(http.MethodGet, "/user/groups", "", token)
		var groups []Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
		require.Len(t, groups, 1, "授予后可见: %s", rec.Body.String())
	}

	// 替换语义：只留 uid1 → uid2 被撤销
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments",
		`{"user_ids":[`+itoa(uid1)+`]}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doUser(http.MethodGet, "/user/groups", "", token2)
	var groups2 []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups2))
	require.Empty(t, groups2, "撤销后不可见: %s", rec.Body.String())

	// 清空（幂等重放授予）
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", `{"user_ids":[]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", `{"user_ids":[]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear idempotent")

	// 用户缺失 → 404；组缺失 → 404
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", `{"user_ids":[99999]}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing user: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/groups/99999/assignments", `{"user_ids":[`+itoa(uid1)+`]}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing group: %s", rec.Body.String())

	// 重复 user_id → 400
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments",
		`{"user_ids":[`+itoa(uid1)+`,`+itoa(uid1)+`]}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "dup user: %s", rec.Body.String())
}

// TestAdminLogsUserFilter /admin/usage_logs user_id 参数绑定（admin 看全部——
// 不传 user_id 不过滤；传了按用户过滤）。
func TestAdminLogsUserFilter(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)

	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: 7, RequestID: "r7", Model: "gpt-4o", Format: domain.FormatOpenAIChat, CreatedAt: base},
		{ID: 2, UserID: 8, RequestID: "r8", Model: "o3", Format: domain.FormatOpenAIResponses, CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	// 不传 user_id → 全部
	rec := doAdmin(http.MethodGet, "/admin/usage_logs?"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 2)

	// user_id=7 → 1 条
	rec = doAdmin(http.MethodGet, "/admin/usage_logs?user_id=7&"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "user filter: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.Equal(t, int64(7), *body.Rows[0].UserID)
}

// TestAdminUserGroups 用户维度分组（GET/PUT /admin/users/{id}/groups）：
// 替换语义（未列出撤销 / 空数组清空）+ 倍率换算（1.5 → 万分数 15000 存储 →
// GET 回显 1.5；null 清除）+ 与组维度 GET /groups/{id}/assignments 交叉验证；
// 非法/缺失 → 400/404。
func TestAdminUserGroups(t *testing.T) {
	doAdmin, doUser, store := newSharedRouters(t)

	// 建两个 private 组 + 注册用户
	var gids []int64
	for _, name := range []string{"u-g1", "u-g2"} {
		rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"`+name+`","visibility":"private"}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "create group: %s", rec.Body.String())
		var g Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
		gids = append(gids, *g.ID)
	}
	token, uid := registerAndGet(t, doUser, "ug@example.com")

	// PUT 授予两组建 g1 专属倍率 1.5 → 响应回显；未设置的组 → null
	body := fmt.Sprintf(`{"group_ids":[%d,%d],"multipliers":{"%d":1.5}}`, gids[0], gids[1], gids[0])
	rec := doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "set groups: %s", rec.Body.String())
	var resp UserGroupsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.ElementsMatch(t, gids, resp.GroupIds)
	require.Equal(t, 1.5, *(*resp.Multipliers)[itoa(gids[0])], "倍率回显")
	require.Nil(t, (*resp.Multipliers)[itoa(gids[1])], "未设置 → null")

	// 存储层换算：1.5 → 万分数 15000
	store.mu.Lock()
	stored := store.assignMult[[2]int64{gids[0], uid}]
	store.mu.Unlock()
	require.NotNil(t, stored)
	require.Equal(t, 15000, *stored, "1.5 → 15000 存储")

	// GET 用户视角回读；GET 组视角交叉验证（用户维度写入 ↔ 组维度读取一致）
	rec = doAdmin(http.MethodGet, "/admin/users/"+itoa(uid)+"/groups", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "get user groups: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.ElementsMatch(t, gids, resp.GroupIds)
	require.Equal(t, 1.5, *(*resp.Multipliers)[itoa(gids[0])])
	rec = doAdmin(http.MethodGet, "/admin/groups/"+itoa(gids[0])+"/assignments", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "get group assignments: %s", rec.Body.String())
	var gresp GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gresp))
	require.Equal(t, []int64{uid}, gresp.UserIds)
	require.Equal(t, 1.5, *(*gresp.Multipliers)[itoa(uid)], "组维度读取与用户维度写入一致")

	// 用户面可见性：授予的 private 组出现在 /user/groups
	rec = doUser(http.MethodGet, "/user/groups", "", token)
	var groups []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 2, "授予后用户面可见")

	// null 清除专属倍率 → 存储层清除、响应 null
	body = fmt.Sprintf(`{"group_ids":[%d,%d],"multipliers":{"%d":null}}`, gids[0], gids[1], gids[0])
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear mult: %s", rec.Body.String())
	resp = UserGroupsResponse{} // 重置：json 复用非 nil map 不清空既有键
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Nil(t, (*resp.Multipliers)[itoa(gids[0])], "null = 清除为未设置")
	store.mu.Lock()
	cleared, ok := store.assignMult[[2]int64{gids[0], uid}]
	store.mu.Unlock()
	require.True(t, ok)
	require.Nil(t, cleared, "存储层已清除")

	// 替换语义：只留 g1 → g2 撤销；空数组 = 清空
	body = fmt.Sprintf(`{"group_ids":[%d]}`, gids[0])
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "replace: %s", rec.Body.String())
	resp = UserGroupsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []int64{gids[0]}, resp.GroupIds)
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", `{"group_ids":[]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear all: %s", rec.Body.String())
	resp = UserGroupsResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.GroupIds)
	require.Empty(t, *resp.Multipliers, "multipliers 为空对象 {}（无专属倍率）")

	// 错误路径：用户缺失 → 404；组缺失 → 404；multipliers 键不在 group_ids → 400；
	// 重复 group_ids → 400；倍率越界 → 400
	rec = doAdmin(http.MethodPut, "/admin/users/99999/groups", body, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing user: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", `{"group_ids":[99999]}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing group: %s", rec.Body.String())
	body = fmt.Sprintf(`{"group_ids":[%d],"multipliers":{"%d":1.0}}`, gids[0], gids[1])
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", body, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "mult key not in group_ids: %s", rec.Body.String())
	body = fmt.Sprintf(`{"group_ids":[%d,%d]}`, gids[0], gids[0])
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", body, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "dup group: %s", rec.Body.String())
	body = fmt.Sprintf(`{"group_ids":[%d],"multipliers":{"%d":10.1}}`, gids[0], gids[0])
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(uid)+"/groups", body, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "mult out of range: %s", rec.Body.String())

	// GET 缺失资源 → 404
	rec = doAdmin(http.MethodGet, "/admin/users/99999/groups", "", "")
	require.Equal(t, http.StatusNotFound, rec.Code, "get missing user: %s", rec.Body.String())
	rec = doAdmin(http.MethodGet, "/admin/groups/99999/assignments", "", "")
	require.Equal(t, http.StatusNotFound, rec.Code, "get missing group: %s", rec.Body.String())
}
