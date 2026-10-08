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

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// newBalanceLogsRouter 分页端点测试接线：暴露 fakeStore 供播种，admin token
// 中间件 + 契约路由；do 传空 token 则不带 Authorization（401 断言）。
func newBalanceLogsRouter(t *testing.T) (*fakeStore, func(method, path, token string) *httptest.ResponseRecorder) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: testEmailCodes})
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
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	return store, do
}

// seedBalanceLogs 播种 N 行记录（id = 1..n，amount 递增）。
func seedBalanceLogs(t *testing.T, store *fakeStore, userID, n int64) {
	t.Helper()
	for i := int64(1); i <= n; i++ {
		note := "n"
		store.balanceLogs = append(store.balanceLogs, &domain.BalanceLog{
			ID: i, UserID: userID, Amount: i * 100, BalanceAfter: i * 100,
			Source: domain.BalanceSourceAdminAdjust, OperatorID: 0, Note: &note,
		})
	}
}

// TestGetUsersIdBalanceLogs 分页/裁剪/换算/404/401（A8）。
func TestGetUsersIdBalanceLogs(t *testing.T) {
	store, do := newBalanceLogsRouter(t)
	u, err := store.CreateUser(context.Background(), &domain.User{
		Email: "logs@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)
	seedBalanceLogs(t, store, u.ID, 3)

	// 分页：limit=2&offset=0 → total=3、rows=2，Amount 毫分 → USD（100 → 0.001）。
	rec := do(http.MethodGet, "/api/admin/users/"+itoa(u.ID)+"/balance-logs?limit=2&offset=0", "admin-tok")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var body BalanceLogListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(3), body.Total)
	require.Len(t, body.Rows, 2)
	// id DESC：id=3（amount 300）在前。
	require.Equal(t, int64(3), body.Rows[0].ID)
	require.InDelta(t, 0.003, body.Rows[0].Amount, 1e-12, "300 毫分 → 0.003 USD")

	// offset 越界 → 空 rows，total 不变。
	rec = do(http.MethodGet, "/api/admin/users/"+itoa(u.ID)+"/balance-logs?offset=99", "admin-tok")
	require.Equal(t, 200, rec.Code)
	body = BalanceLogListResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(3), body.Total)
	require.Empty(t, body.Rows)

	// limit 超限裁剪到 200（不报错）——ClampLimit。
	rec = do(http.MethodGet, "/api/admin/users/"+itoa(u.ID)+"/balance-logs?limit=100000", "admin-tok")
	require.Equal(t, 200, rec.Code, rec.Body.String())

	// 用户缺失 → 404。
	rec = do(http.MethodGet, "/api/admin/users/999999/balance-logs", "admin-tok")
	require.Equal(t, 404, rec.Code, "用户缺失 → 404: %s", rec.Body.String())

	// 非 admin（无 token）→ 401。
	rec = do(http.MethodGet, "/api/admin/users/"+itoa(u.ID)+"/balance-logs", "")
	require.Equal(t, 401, rec.Code)

	// 非法 query 绑定（limit 非整数）→ 400。
	rec = do(http.MethodGet, "/api/admin/users/"+itoa(u.ID)+"/balance-logs?limit=abc", "admin-tok")
	require.Equal(t, 400, rec.Code, "非法参数格式 → 400: %s", rec.Body.String())
}

// TestToAPIBalanceLog 换算：有符号毫分 → USD；Source/Note/OperatorID 透传。
func TestToAPIBalanceLog(t *testing.T) {
	note := "ABC-1234"
	l := &domain.BalanceLog{
		ID: 5, UserID: 7, Amount: -300, BalanceAfter: 700,
		Source: domain.BalanceSourceRedemption, OperatorID: 42, Note: &note,
	}
	api := toAPIBalanceLog(l)
	require.Equal(t, int64(5), api.ID)
	require.Equal(t, int64(7), api.UserID)
	require.InDelta(t, -0.003, api.Amount, 1e-12)
	require.InDelta(t, 0.007, api.BalanceAfter, 1e-12)
	require.Equal(t, BalanceLogSource("redemption"), api.Source)
	require.Equal(t, int64(42), api.OperatorID)
	require.NotNil(t, api.Note)
	require.Equal(t, "ABC-1234", *api.Note)
}
