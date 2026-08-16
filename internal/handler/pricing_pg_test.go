// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// 真实 PG 集成基座（与 internal/pricing 包同款约定）：
//   TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test \
//     go test ./internal/handler/ -run PG -v
// 未设置 TEST_DATABASE_URL → t.Skip。每测试重建独立 schema（handler_pricing_test）：
// 与 repository 包（public）、pricing 包（pricing_test）等隔离——并发跑不互踩。

// handlerPricingPGTestSchema 本包 PG 测试专用 schema（同一数据库内隔离命名空间）。
const handlerPricingPGTestSchema = "handler_pricing_test"

// newPricingPGRepos 打开真实 PG（独立 schema）+ ent 迁移建表，返回仓库。
func newPricingPGRepos(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	// search_path 指向独立 schema：连接池每连接的默认命名空间（pgx runtime
	// param）；ent 迁移与查询按 search_path 解析，不触碰 public。
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + handlerPricingPGTestSchema
	} else {
		dsn += "?search_path=" + handlerPricingPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+handlerPricingPGTestSchema+` CASCADE; CREATE SCHEMA `+handlerPricingPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	return repos
}

// newPricingPGRouter 真实 PG + 契约路由接线（admin token 中间件，同
// newListTestRouter；svc 快照重载走真实仓库）。
func newPricingPGRouter(t *testing.T) (*AdminAPI, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	repos := newPricingPGRepos(t)
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
	return h, do
}

// TestPricingModelSlashPG 含 `/` 模型名回归（钉死缺陷）：模型名是自由字符串
// （可含多段 `/` 与 `:`），路径参数单段匹配会拆段 404——model 走 query 后
// PUT → list 精确筛选回显 → DELETE 全链路可用。三价格表同链各跑一遍
// （pricing/image-price/function-prices 的 PUT/DELETE 均 query 化）。
func TestPricingModelSlashPG(t *testing.T) {
	_, do := newPricingPGRouter(t)

	// 用户实测触发 404 的模型名形态；query 值 URL 编码（%2F）——真实客户端
	// （URLSearchParams）即如此发送，服务端 r.URL.Query() 正确还原。
	model := "1024-x-1024/50-steps/bedrock/amazon.nova-canvas-v1:0"
	enc := "model=" + url.QueryEscape(model)

	// --- pricing：PUT → list 精确筛选命中 → DELETE ---
	rec := do(http.MethodPut, "/admin/pricing?"+enc, `{"prompt_price_per_million":0.001,"completion_price_per_million":0.002}`)
	require.Equal(t, 200, rec.Code, "pricing put: %s", rec.Body.String())
	var p Pricing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, model, p.Model, "PUT 回显含斜杠模型名")

	rec = do(http.MethodGet, "/admin/pricing?"+enc, "")
	require.Equal(t, 200, rec.Code, "pricing list: %s", rec.Body.String())
	var pl PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pl))
	require.Equal(t, int64(1), pl.Total, "list 精确筛选命中")
	require.Equal(t, model, pl.Rows[0].Model, "list 回显含斜杠模型名")
	require.Equal(t, 0.001, pl.Rows[0].PromptPricePerMillion, "价格 roundtrip")

	rec = do(http.MethodDelete, "/admin/pricing?"+enc, "")
	require.Equal(t, 200, rec.Code, "pricing delete: %s", rec.Body.String())

	rec = do(http.MethodGet, "/admin/pricing?"+enc, "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pl))
	require.Zero(t, pl.Total, "删除后列表精确筛选为空")

	// --- image-price：同链 ---
	rec = do(http.MethodPut, "/admin/image-price?"+enc, `{"output_cost_per_image":0.05}`)
	require.Equal(t, 200, rec.Code, "image-price put: %s", rec.Body.String())
	var ip ImagePrice
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ip))
	require.Equal(t, model, ip.Model)

	rec = do(http.MethodGet, "/admin/image-price?"+enc, "")
	require.Equal(t, 200, rec.Code, "image-price list: %s", rec.Body.String())
	var il ImagePriceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &il))
	require.Equal(t, int64(1), il.Total, "image-price list 精确筛选命中")
	require.Equal(t, model, il.Rows[0].Model)

	rec = do(http.MethodDelete, "/admin/image-price?"+enc, "")
	require.Equal(t, 200, rec.Code, "image-price delete: %s", rec.Body.String())

	// --- function-prices：同链 ---
	rec = do(http.MethodPut, "/admin/function-prices?"+enc, `{"price_per_call":0.01}`)
	require.Equal(t, 200, rec.Code, "function-prices put: %s", rec.Body.String())
	var fp FunctionPrice
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fp))
	require.Equal(t, model, fp.Model)

	rec = do(http.MethodGet, "/admin/function-prices?"+enc, "")
	require.Equal(t, 200, rec.Code, "function-prices list: %s", rec.Body.String())
	var fl FunctionPriceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fl))
	require.Equal(t, int64(1), fl.Total, "function-prices list 精确筛选命中")
	require.Equal(t, model, fl.Rows[0].Model)

	rec = do(http.MethodDelete, "/admin/function-prices?"+enc, "")
	require.Equal(t, 200, rec.Code, "function-prices delete: %s", rec.Body.String())
}
