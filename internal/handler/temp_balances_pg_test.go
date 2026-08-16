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
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// /admin/temp-balances 真实 PG 测试（spec 2026-08-15）：全量视角（含过期/
// 用尽/负扣减行）+ user_id 筛选 + sort/order 白名单 + 分页 total + USD 换算。
// 基座同 overview_pg_test.go：独立 schema + repository.New（本查询走 ent，
// 无需 pool/分区表）。

// handlerTempBalancesPGTestSchema 本文件 PG 测试专用 schema。
const handlerTempBalancesPGTestSchema = "handler_tempbalances_test"

// tempBalancesPGTestDB 打开真实 PG（独立 schema）+ 迁移建表，返回仓库。
func tempBalancesPGTestDB(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + handlerTempBalancesPGTestSchema
	} else {
		dsn += "?search_path=" + handlerTempBalancesPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+handlerTempBalancesPGTestSchema+` CASCADE; CREATE SCHEMA `+handlerTempBalancesPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	return repos
}

// newTempBalancesPGRouter 真实 PG + 契约路由（admin token 中间件）。
func newTempBalancesPGRouter(t *testing.T) (*repository.Repository, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	repos := tempBalancesPGTestDB(t)
	svc := service.New(repos, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
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
	return repos, do
}

// seedTempBalancesPG 建用户 + 临时额度行（amount 毫分；返回 (userID, 行数)）。
func seedTempBalancesPG(t *testing.T, repos *repository.Repository, email string, rows ...struct {
	amount    int64
	expiresAt *time.Time
	note      *string
}) int64 {
	t.Helper()
	u, err := repos.CreateUser(context.Background(), domainUser(email))
	require.NoError(t, err)
	for _, r := range rows {
		require.NoError(t, repos.CreateTempBalance(context.Background(), u.ID, r.amount, r.expiresAt, r.note))
	}
	return u.ID
}

func domainUser(email string) *domain.User {
	return &domain.User{
		Email: email, PasswordHash: "bcrypt-hash-" + email,
		Role: domain.RoleUser, Status: domain.UserStatusActive, MaxConcurrency: 0,
	}
}

// TestPGAdminTempBalances 全量视角 + 分页 + 筛选 + 排序 + USD 换算。
func TestPGAdminTempBalances(t *testing.T) {
	repos, do := newTempBalancesPGRouter(t)

	now := time.Now().Truncate(time.Second)
	noteBonus := "signup bonus"
	noteRedeem := "redemption code"
	u1 := seedTempBalancesPG(t, repos, "tb1@example.com",
		struct {
			amount    int64
			expiresAt *time.Time
			note      *string
		}{amount: 300000, expiresAt: ptrT(now.AddDate(0, 0, 5)), note: &noteBonus},
		struct {
			amount    int64
			expiresAt *time.Time
			note      *string
		}{amount: 200000, expiresAt: ptrT(now.AddDate(0, 0, -1)), note: &noteRedeem}, // 已过期
		struct {
			amount    int64
			expiresAt *time.Time
			note      *string
		}{amount: 0, expiresAt: ptrT(now.AddDate(0, 0, 30)), note: &noteBonus}, // 已用尽
	)
	u2 := seedTempBalancesPG(t, repos, "tb2@example.com",
		struct {
			amount    int64
			expiresAt *time.Time
			note      *string
		}{amount: 50000, expiresAt: nil, note: &noteRedeem}, // 永久
	)

	// 全量视角：两用户 4 行全可见（含过期/用尽）；默认 expires_at asc（FEFO
	// 同序）→ 已过期(-1d) 先、永久最后；amount_usd = 毫分/1e5。
	rec := do(http.MethodGet, "/admin/temp-balances", "")
	require.Equal(t, http.StatusOK, rec.Code, "list: %s", rec.Body.String())
	var resp AdminTempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, int64(4), resp.Total)
	require.Len(t, resp.Rows, 4)
	require.Equal(t, float64(2.0), resp.Rows[0].AmountUsd, "过期行可见且最早到期在前（毫分 200000 → USD 2.0）")
	require.Equal(t, float64(3.0), resp.Rows[1].AmountUsd)
	require.Equal(t, float64(0.0), resp.Rows[2].AmountUsd, "用尽行可见")
	require.Nil(t, resp.Rows[3].ExpiresAt, "永久最后")
	require.Equal(t, u1, resp.Rows[0].UserId)
	require.Equal(t, "redemption code", *resp.Rows[3].Note)

	// user_id 筛选
	rec = do(http.MethodGet, fmt.Sprintf("/admin/temp-balances?user_id=%d", u2), "")
	require.Equal(t, http.StatusOK, rec.Code)
	var filtered AdminTempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &filtered))
	require.Equal(t, int64(1), filtered.Total)
	require.Len(t, filtered.Rows, 1)
	require.Equal(t, u2, filtered.Rows[0].UserId)

	// 分页：page_size=2 → 行集 2、total 恒 4；page=2 → 后两行
	rec = do(http.MethodGet, "/admin/temp-balances?page=1&page_size=2", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var p1 AdminTempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p1))
	require.Equal(t, int64(4), p1.Total, "total = 满足筛选总数，不分页裁剪")
	require.Len(t, p1.Rows, 2)
	rec = do(http.MethodGet, "/admin/temp-balances?page=2&page_size=2", "")
	var p2 AdminTempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p2))
	require.Equal(t, int64(4), p2.Total)
	require.Len(t, p2.Rows, 2)
	require.Equal(t, float64(0.0), p2.Rows[0].AmountUsd, "第 2 页首行 = FEFO 序第 3 行（用尽行）")
	require.Nil(t, p2.Rows[1].ExpiresAt, "第 2 页末行 = 永久行（FEFO 序最后）")

	// sort=amount desc + order=desc（显式）
	rec = do(http.MethodGet, "/admin/temp-balances?sort=amount&order=desc", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var byAmount AdminTempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &byAmount))
	require.Equal(t, float64(3.0), byAmount.Rows[0].AmountUsd, "amount desc 最大在前")

	// sort=created_at 白名单键可用（order=asc 显式）
	rec = do(http.MethodGet, "/admin/temp-balances?sort=created_at&order=asc", "")
	require.Equal(t, http.StatusOK, rec.Code)

	// 非法输入 → 400：sort 白名单外 / order 非法 / page_size 越界
	for _, path := range []string{
		"/admin/temp-balances?sort=id",
		"/admin/temp-balances?sort=amount&order=sideways",
		"/admin/temp-balances?page_size=0",
		"/admin/temp-balances?page_size=1001",
	} {
		rec = do(http.MethodGet, path, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", path, rec.Body.String())
	}

	// 空结果（不存在的 user_id 筛选）：total 0 + 空数组（非 null）
	rec = do(http.MethodGet, "/admin/temp-balances?user_id=999999", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var empty AdminTempBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &empty))
	require.Equal(t, int64(0), empty.Total)
	require.Empty(t, empty.Rows)
	require.Contains(t, rec.Body.String(), `"rows":[]`, "rows 序列化为空数组而非 null")
}

// ptrT 时间指针（nil = 永久额度）。
func ptrT(t time.Time) *time.Time { return &t }
