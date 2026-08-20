// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// /api/user/temp-balances + /api/user/auth/change-password 真实 PG 测试（spec
// 2026-08-15）：有效过滤/FEFO 排序/total USD/越权回归/空结果；改密码
// 登录语义校验 + 新密码校验 + 更新后旧密登录失败新密成功。

// handlerUserTempPGTestSchema 本文件 PG 测试专用 schema。
const handlerUserTempPGTestSchema = "handler_usertemp_test"

// newUserTempPGRouter 真实 PG 用户面路由（真实 svc + 真实 Issuer；状态快照
// 直读真实仓库——对齐 fakeUserStatus 语义）。
func newUserTempPGRouter(t *testing.T) (*repository.Repository, func(method, path, body, token string) *httptest.ResponseRecorder) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + handlerUserTempPGTestSchema
	} else {
		dsn += "?search_path=" + handlerUserTempPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+handlerUserTempPGTestSchema+` CASCADE; CREATE SCHEMA `+handlerUserTempPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	svc := service.New(repos, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	iss := auth.NewIssuer("test-secret")
	router := userapi.Router(svc, iss, pgUserStatus{repos: repos}, nil)
	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	return repos, do
}

// registerPGUser 经注册端点建用户，返回 (JWT, 用户 id)。
func registerPGUser(t *testing.T, do func(method, path, body, token string) *httptest.ResponseRecorder, email, password string) (string, int64) {
	t.Helper()
	rec := do(http.MethodPost, "/api/user/auth/register",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.User.ID)
	return resp.Token, *resp.User.ID
}

// TestPGUserTempBalances 有效过滤 + FEFO 排序 + total USD + 越权回归 + 空结果。
func TestPGUserTempBalances(t *testing.T) {
	repos, do := newUserTempPGRouter(t)
	ctx := context.Background()

	token, uid := registerPGUser(t, do, "tbuser@example.com", "s3cret-pass")
	now := time.Now().Truncate(time.Second)
	noteBonus := "signup bonus"
	noteRedeem := "redemption code"
	// 有效：2 天后到期（先）/ 5 天后到期（次）/ 永久（最后）；无效：过期/用尽/负
	for _, r := range []struct {
		amount    int64
		expiresAt *time.Time
		note      *string
	}{
		{amount: 300000, expiresAt: ptrT(now.AddDate(0, 0, 5)), note: &noteBonus},
		{amount: 150000, expiresAt: ptrT(now.AddDate(0, 0, 2)), note: &noteRedeem},
		{amount: 500000, expiresAt: nil, note: &noteBonus},
		{amount: 400000, expiresAt: ptrT(now.AddDate(0, 0, -1)), note: &noteBonus},  // 已过期 → 隐藏
		{amount: 0, expiresAt: ptrT(now.AddDate(0, 0, 30)), note: &noteRedeem},     // 已用尽 → 隐藏
		{amount: -100000, expiresAt: ptrT(now.AddDate(0, 0, 30)), note: &noteRedeem}, // 负扣减 → 隐藏
	} {
		require.NoError(t, repos.CreateTempBalance(ctx, uid, r.amount, r.expiresAt, r.note))
	}

	rec := do(http.MethodGet, "/api/user/temp-balances", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "list: %s", rec.Body.String())
	var resp userapi.TempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Rows, 3, "仅有效行：过期/用尽/负扣减隐藏")
	// FEFO 同序：expires_at 升序 + 永久最后
	require.Equal(t, float64(1.5), resp.Rows[0].AmountUsd, "毫分 150000 → USD 1.5")
	require.Equal(t, float64(3.0), resp.Rows[1].AmountUsd)
	require.Nil(t, resp.Rows[2].ExpiresAt, "永久最后")
	require.Equal(t, float64(5.0), resp.Rows[2].AmountUsd)
	// total_usd = 有效毫分 Σ /1e5（9.5）
	require.Equal(t, float64(9.5), resp.TotalUsd, "合计 = 150000+300000+500000 毫分 /1e5")
	// note 暴露（固定系统备注）
	require.Equal(t, "redemption code", *resp.Rows[0].Note)
	require.NotNil(t, resp.Rows[1].ExpiresAt)

	// 越权回归：契约无 user_id 参数——query 带他人 user_id 被忽略（仍返回本人数据）
	rec = do(http.MethodGet, "/api/user/temp-balances?user_id=999999", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "user_id 参数无效（忽略）: %s", rec.Body.String())
	var again userapi.TempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &again))
	require.Equal(t, len(resp.Rows), len(again.Rows), "查询参数不影响结果")

	// 空结果（另一用户无有效额度）：total_usd 0 + rows 空数组（不 404、非 null）
	registerPGUser(t, do, "tbuser2@example.com", "s3cret-pass")
	rec = do(http.MethodGet, "/api/user/temp-balances", "", token2For(t, do, "tbuser2@example.com", "s3cret-pass"))
	require.Equal(t, http.StatusOK, rec.Code)
	var empty userapi.TempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &empty))
	require.Equal(t, float64(0), empty.TotalUsd)
	require.Empty(t, empty.Rows)
	require.Contains(t, rec.Body.String(), `"rows":[]`, "rows 序列化为空数组而非 null")

	// 无 token → 401
	rec = do(http.MethodGet, "/api/user/temp-balances", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// token2For 二次登录取 JWT（空结果断言用第二个用户）。
func token2For(t *testing.T, do func(method, path, body, token string) *httptest.ResponseRecorder, email, password string) string {
	t.Helper()
	rec := do(http.MethodPost, "/api/user/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), "")
	require.Equal(t, http.StatusOK, rec.Code, "login: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Token
}

// TestPGUserChangePassword 改密码全链：正确 200 → 旧密登录 401 新密 200；
// 旧密错误 401；新密空/超长 400；既有 JWT 不撤销。
func TestPGUserChangePassword(t *testing.T) {
	repos, do := newUserTempPGRouter(t)

	token, uid := registerPGUser(t, do, "cp@example.com", "old-pass-1")

	// 旧密错误 → 401（同登录文案防枚举）
	rec := do(http.MethodPost, "/api/user/auth/change-password",
		`{"old_password":"wrong-pass","new_password":"new-pass-1"}`, token)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "wrong old: %s", rec.Body.String())

	// 新密空 → 400；新密超长（73 字节）→ 400
	rec = do(http.MethodPost, "/api/user/auth/change-password",
		`{"old_password":"old-pass-1","new_password":""}`, token)
	require.Equal(t, http.StatusBadRequest, rec.Code, "empty new: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/user/auth/change-password",
		`{"old_password":"old-pass-1","new_password":"`+strings.Repeat("a", 73)+`"}`, token)
	require.Equal(t, http.StatusBadRequest, rec.Code, "long new: %s", rec.Body.String())

	// 正确修改 → 200 {"updated": true}
	rec = do(http.MethodPost, "/api/user/auth/change-password",
		`{"old_password":"old-pass-1","new_password":"new-pass-1"}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "change: %s", rec.Body.String())
	var chg userapi.ChangePasswordResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &chg))
	require.True(t, chg.Updated)

	// 更新后：旧密登录 401、新密登录 200
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"cp@example.com","password":"old-pass-1"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "old password login must fail: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"cp@example.com","password":"new-pass-1"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "new password login: %s", rec.Body.String())

	// 既有 JWT 不撤销（无状态 token 无撤销机制——新密码下次登录生效）：
	// 改密前的 token 仍可用 /api/user/auth/me
	rec = do(http.MethodGet, "/api/user/auth/me", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "old token still valid: %s", rec.Body.String())
	require.Equal(t, uid, *mustMe(t, rec).ID)

	// 落库 hash 已更新（非明文存储——防泄密回归）
	u, err := repos.GetUser(context.Background(), uid)
	require.NoError(t, err)
	require.NotEqual(t, "old-pass-1", u.PasswordHash, "hash 落库")
}

// mustMe 解析 /api/user/auth/me 响应（改密后旧 token 仍可用断言）。
func mustMe(t *testing.T, rec *httptest.ResponseRecorder) *userapi.User {
	t.Helper()
	var me userapi.User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	return &me
}
