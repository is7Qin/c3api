// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler_test

// request_patch_admin_pg_test.go — P2 的 **admin + PG 端到端**证据
// （spec custom-request-patch §10 P2 erratum 交接 / §17.2）。
//
// 覆盖完整生产链路（不是分段夹具）：
//   admin HTTP（端点级 UseNumber 解码）→ service 校验 → repository 守卫写 →
//   PG jsonb（numeric 规范化）→ repository.LoadGroupAccounts（mapping 的 custom 门
//   Paths+Patch 同源同门）→ domain.Template.Upstream → 唯一构造函数
//   newSnapshotStatic / digestTemplateUpstream。
//
// 为什么放在 external test package：本包测试要 import handler/service（真实管理面
// HTTP），而 handler→service→scheduler 依赖方向使内部测试包产生 import cycle。
// digest 与键槽经 export_test.go 暴露，但断言的仍是生产实现本身。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"

	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// adminPGEmailCodes 是 service.New 必选依赖的无行为替身（本用例不触验证码面）。
type adminPGEmailCodes struct{}

func (adminPGEmailCodes) GetEmailCode(context.Context, string, string) (*domain.EmailCode, error) {
	return nil, nil
}

func (adminPGEmailCodes) UpsertEmailCode(context.Context, string, string, string, time.Time) (*domain.EmailCode, error) {
	return nil, nil
}

func (adminPGEmailCodes) IncrementEmailCodeAttempts(context.Context, string, string) (int, error) {
	return 0, nil
}

func (adminPGEmailCodes) DeleteEmailCode(context.Context, string, string) error { return nil }

// TestPGAdminRequestPatchToSnapshotDigest 是 §17.2 的 admin+PG 端到端：
// 管理面写 request_patch（1e18 / >2^53 整数 / {api_key} 占位符）→ 真实 PG 读回
// mapping → digest 稳定且随配置变化、大整数规范化不导致摘要抖动。
func TestPGAdminRequestPatchToSnapshotDigest(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), false, pool)
	require.NoError(t, err)

	// 真实 service + handler 装配（与 main 同构；与本用例无关的依赖省略）。
	svc := service.New(service.Deps{
		Store: repos, Invalidate: service.NopInvalidator{}, EmailCodeStore: adminPGEmailCodes{},
	})
	h := handler.New(svc)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	router.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// 1) 管理面创建 custom 模板。
	rec := do(http.MethodPost, "/api/admin/templates", `{
		"name":"pg-admin-rp","base_url":"https://u.example","credential_type":"custom",
		"supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, rec.Code, "create template: %s", rec.Body.String())
	var tpl handler.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	tplPath := "/api/admin/templates/" + strconv.FormatInt(tpl.ID, 10) + "/ext"

	// 2) 管理面 PUT request_patch：端点级 UseNumber 保 9007199254740993，1e18 由
	//    PG jsonb 规范化为十进制；写面经受 service 校验 + 唯一 ParseRequestPatch 入口。
	putBody := `{"credential_type":"custom",
		"upstream_paths":{"openai-chat":"v1/custom/chat?api-version=2024-10-21"},
		"request_patch":{
		  "headers":{"Authorization":"Bearer {api_key}","X-Call-Source":"vibe_code"},
		  "formats":{"openai-chat":{
		    "headers":{"User-Agent":"vibe-code/1.0"},
		    "set":{"big":9007199254740993,"e18":1e18,
		           "nested":{"n":9007199254740993,"arr":[1,2]}},
		    "delete":["metadata.client_field"]}}}}`
	rec = do(http.MethodPut, tplPath, putBody)
	require.Equal(t, http.StatusOK, rec.Code, "put request_patch: %s", rec.Body.String())
	putResp := rec.Body.String()
	require.Contains(t, putResp, "1000000000000000000",
		"真实链路响应来自 jsonb 读回：1e18 必须已展开为十进制")
	require.NotContains(t, putResp, "1e18", "PUT 响应不得回显入参指数字面量（读回路径）")
	require.Contains(t, putResp, "9007199254740993", ">2^53 整数逐字保留")
	require.Contains(t, putResp, "{api_key}", "占位符原样（不替换/不掩码）")

	// 3) 建组/账号（管理面建号与本用例无关），走**真实载入器**读回 mapping。
	g, err := repos.CreateGroup(ctx, &domain.Group{
		Name: "g-admin-rp", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	acc, err := repos.CreateAccount(ctx, &domain.Account{
		Name: "a-admin-rp", TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
		MaxConcurrency: 4, Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

	load := func(t *testing.T) (*domain.Account, *domain.Template) {
		t.Helper()
		accs, lerr := repos.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, lerr)
		require.Len(t, accs, 1)
		require.NotNil(t, accs[0].Template)
		return accs[0], accs[0].Template
	}

	firstAcc, first := load(t)
	require.NotNil(t, first.Upstream, "custom + ext 行 ⇒ 快照 Template.Upstream 必须非 nil")
	require.NotNil(t, first.Upstream.Patch, "mapping 必须把读回的 request_patch 解析进 Upstream.Patch")
	require.Equal(t, "v1/custom/chat?api-version=2024-10-21",
		first.Upstream.Paths[string(domain.UpstreamPathChat)], "paths 与 patch 同源同门")
	readBytes := string(first.Upstream.Patch.CanonicalBytes())
	require.Contains(t, readBytes, "1000000000000000000", "读回的 CanonicalBytes 是 PG 规范化后的十进制")
	require.NotContains(t, readBytes, "1e18")

	// 4) digest 用读回值：同一 DB 值两次读回必须同摘要（无抖动），且等于键槽。
	d1 := scheduler.DigestTemplateUpstreamForTest(first.Upstream)
	_, second := load(t)
	d2 := scheduler.DigestTemplateUpstreamForTest(second.Upstream)
	require.Equal(t, d1, d2,
		"同一 DB 值两次读回的 digest 必须逐字节相同（大整数规范化不导致抖动）")
	key1 := scheduler.SnapshotUpstreamKeyForTest(*firstAcc, first, []int64{g.ID})
	require.Equal(t, d1, key1, "唯一构造函数的 upstreamPathsKey 必须等于 digestTemplateUpstream")

	// 5) 管理面改配置 ⇒ 读回摘要变、planKey 槽变（配置变更换新计划）。
	rec = do(http.MethodPut, tplPath, `{"credential_type":"custom",
		"upstream_paths":{"openai-chat":"v1/custom/chat?api-version=2024-10-21"},
		"request_patch":{"headers":{"X-Call-Source":"changed"}}}`)
	require.Equal(t, http.StatusOK, rec.Code, "second put: %s", rec.Body.String())
	thirdAcc, third := load(t)
	d3 := scheduler.DigestTemplateUpstreamForTest(third.Upstream)
	require.NotEqual(t, d1, d3, "管理面改 request_patch 后读回 digest 必须变")
	require.NotEqual(t, key1, scheduler.SnapshotUpstreamKeyForTest(*thirdAcc, third, []int64{g.ID}),
		"改 request_patch 必须换新 planKey 槽位（否则复用旧叶，旧配置继续生效）")
	require.NotContains(t, string(third.Upstream.Patch.CanonicalBytes()), "{api_key}",
		"旧配置已被全列替换（PUT 语义：缺省字段 = 清空）")

	// 6) 非 custom 提交非空 ⇒ 400（custom-only 门不变；P1 已锁，这里确认真实 PG
	//    装配下行为一致）。
	rec = do(http.MethodPost, "/api/admin/templates", `{
		"name":"pg-admin-api-key","base_url":"https://u.example","credential_type":"api_key",
		"supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var plain handler.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plain))
	rec = do(http.MethodPut, "/api/admin/templates/"+strconv.FormatInt(plain.ID, 10)+"/ext",
		`{"credential_type":"api_key","request_patch":{"headers":{"X-A":"1"}}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "非 custom 非空 request_patch 必须 400")
}
