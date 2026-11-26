// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// ext_request_patch_pg_test.go — request_patch 的**真实 PG 链路**证据
// （spec §11.11 P1 erratum / §17.1；评审 oracle I1）。
//
// 为什么必须 PG：TestTemplatesIdExtRequestPatch 用 fakeStore，只证 handler 端点级
// UseNumber；真实链路是 PUT decodeLenientUseNumber → domain.ParseRequestPatch →
// repository 守卫写（事务内写入 + **事务内回读**）→ PG jsonb（numeric）→
// CanonicalBytes 回显。jsonb 读回会把指数写法展开成十进制：
// 1e18 → 1000000000000000000（语义/精度不变），>2^53 整数字面量（9007199254740993）
// 逐字保留。本用例锁定：
//  1. 9007199254740993 / -9223372036854775808 / 嵌套对象内大整数逐字回显；
//  2. {api_key} 占位符逐字回显（不替换、不掩码）；
//  3. 1e18 回显为 1000000000000000000，按**数值语义**与写入值相等（big.Rat 比较，
//     不比较字面量——这正是 §11.11 erratum 的口径）；
//  4. 真实 jsonb 读回路径确实参与：直接查 template_exts.request_patch::text 断言
//     PG 侧已是展开十进制，且 PUT 响应 = GET 响应（两者都是读回值，不是入参回显）。
//
// 没有 PG DSN 时 pgtest.Clone 直接 t.Skip（skip ≠ green）；CI 打开 require-PG 开关
// 时缺 DSN 直接 fail。环境变量名只在 internal/testsupport/pgtest 出现（单一 owner，
// dsn_source_gate_test.go 是那段纪律的可执行门禁）——本文件连注释都不得拼写它。

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"

	// Registers pgtest repository-dependent hooks (migrate + clone day partitions).
	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// decodeTemplateExtUseNumber 用 json.Decoder.UseNumber 解 GET/PUT 响应：生成的
// TemplateFormatPatch.Set 是 map[string]interface{}，只有 json.Number 能保住
// 大整数字面量（默认 float64 会把 9007199254740993 变成 …992）。
func decodeTemplateExtUseNumber(t *testing.T, body []byte) TemplateExt {
	t.Helper()
	var ext TemplateExt
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&ext))
	return ext
}

// requireSetNumber 取出 set 值的数字字面量（UseNumber 解码 ⇒ json.Number）。
func requireSetNumber(t *testing.T, value any) string {
	t.Helper()
	n, ok := value.(json.Number)
	require.True(t, ok, "set 值必须以 json.Number 解码（端点级 UseNumber 回显），got %T", value)
	return n.String()
}

// requireSameNumericValue 按数值语义比较两个数字字面量（big.Rat；不比较拼写）。
func requireSameNumericValue(t *testing.T, want, got string) {
	t.Helper()
	wantRat, ok := new(big.Rat).SetString(want)
	require.True(t, ok, "want %q", want)
	gotRat, ok := new(big.Rat).SetString(got)
	require.True(t, ok, "got %q", got)
	require.Zero(t, wantRat.Cmp(gotRat), "数值语义必须相等：%q vs %q", want, got)
}

// assertChatSet 断言 openai-chat 格式层的 set 值（大整数逐字 / 指数数值等价 / 嵌套）。
func assertChatSet(t *testing.T, ext TemplateExt, label string) {
	t.Helper()
	require.NotNil(t, ext.RequestPatch, label)
	require.NotNil(t, ext.RequestPatch.Formats, "%s：多格式层必须回显", label)
	chat, ok := (*ext.RequestPatch.Formats)["openai-chat"]
	require.True(t, ok, "%s：openai-chat 层必须回显", label)
	require.NotNil(t, chat.Set, "%s：set 必须回显", label)
	set := *chat.Set

	require.Equal(t, "9007199254740993", requireSetNumber(t, set["big"]),
		"%s：>2^53 整数字面量必须逐字保留（float64 会变 …992）", label)
	require.Equal(t, "-9223372036854775808", requireSetNumber(t, set["neg"]),
		"%s：int64 下界必须逐字保留", label)

	// 指数写法：jsonb numeric 规范化 ⇒ 回显展开十进制；语义必须相等。
	e18 := requireSetNumber(t, set["e18"])
	require.Equal(t, "1000000000000000000", e18,
		"%s：jsonb 必须把 1e18 展开成十进制（表示法，不是精度）", label)
	requireSameNumericValue(t, "1e18", e18)

	nested, ok := set["nested"].(map[string]any)
	require.True(t, ok, "%s：嵌套对象必须回显为对象", label)
	require.Equal(t, "9007199254740993", requireSetNumber(t, nested["n"]),
		"%s：嵌套对象内大整数同样逐字保留", label)

	require.Equal(t, "Bearer {api_key}", (*ext.RequestPatch.Headers)["Authorization"],
		"%s：{api_key} 占位符必须原样回显（不替换/不掩码）", label)
}

// TestTemplatesIdExtRequestPatchPG 真实 PG 链路 PUT→jsonb→GET。
func TestTemplatesIdExtRequestPatchPG(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB("postgres", db), false, pool)
	require.NoError(t, err)

	// 真实 service + handler 装配（与 main 同构；只省略与本用例无关的依赖）。
	svc := service.New(service.Deps{
		Store:          repos,
		Invalidate:     service.NopInvalidator{},
		EmailCodeStore: testEmailCodes,
	})
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

	// 1) 建 custom 模板（真实写入 templates 表）。
	rec := do(http.MethodPost, "/api/admin/templates", `{
		"name":"pg-request-patch","base_url":"https://u.example","credential_type":"custom",
		"supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, rec.Code, "create template: %s", rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	// 2) PUT ext：真实 repository 守卫写 → jsonb（事务内回读）。
	putBody := `{"credential_type":"custom","request_patch":{
	  "headers":{"Authorization":"Bearer {api_key}"},
	  "formats":{"openai-chat":{
	    "set":{"big":9007199254740993,"neg":-9223372036854775808,"e18":1e18,
	           "nested":{"n":9007199254740993,"arr":[1,2]}},
	    "delete":["metadata.client_field"]}}}}`
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "put request_patch: %s", rec.Body.String())
	putResp := rec.Body.String()
	putExt := decodeTemplateExtUseNumber(t, rec.Body.Bytes())
	assertChatSet(t, putExt, "PUT 响应")
	require.NotContains(t, putResp, "1e18",
		"真实链路 PUT 响应来自事务内 jsonb 回读，不得回显入参字面量 1e18")
	require.NotContains(t, putResp, "9007199254740992",
		"大整数不得被 float64 截断")

	// 3) 真实 jsonb 读回路径确实参与：直接查列（jsonb::text）确认 PG 侧数字已被
	//    numeric 规范化。这是本用例与 fakeStore 用例的本质区别。
	var dbRaw string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT request_patch::text FROM template_exts WHERE template_id = $1`, tpl.ID).Scan(&dbRaw))
	require.Contains(t, dbRaw, "1000000000000000000", "PG jsonb 列必须已把 1e18 展开成十进制")
	require.NotContains(t, dbRaw, "1e18", "jsonb 列不保留指数字面量")
	require.Contains(t, dbRaw, "9007199254740993", "JSONB 列必须逐字保存 >2^53 整数")
	require.Contains(t, dbRaw, "{api_key}", "jsonb 列保存占位符原文")
	// 直接用 DB 原始 jsonb 文本构造回显期望：证明 GET/PUT 的值来自读回路径。
	var dbPatch TemplateRequestPatch
	dec := json.NewDecoder(bytes.NewReader([]byte(dbRaw)))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&dbPatch))
	dbExt := TemplateExt{RequestPatch: &dbPatch}
	assertChatSet(t, dbExt, "jsonb 原始文本")

	// 4) GET 与 PUT 响应逐字一致（两者都是读回值；GET 是独立事务再读一次）。
	rec = do(http.MethodGet, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", "")
	require.Equal(t, http.StatusOK, rec.Code, "get request_patch: %s", rec.Body.String())
	require.Equal(t, putResp, rec.Body.String(),
		"GET 必须与 PUT 响应逐字一致（同一 jsonb 读回值；fakeStore 掩盖的正是这一步）")
}
