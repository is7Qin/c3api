// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTemplateExtCodexTransport 模板 ext 的出站传输写面与回显面（spec §4.2/§4.3
// 的 handler 侧契约）：字段灌入 + 恒回显 + 409 降级门可达 + 脏值 400 + 非 codex
// 只接受 "" + 陈旧行幂等例外。
//
// 这条用例的首要目的是钉住**跨批交接**：decodeLenient 宽松解码不会替我们把契约
// 字段搬进领域对象，若 handler 漏掉灌入，PUT 恒落 ""——本用例的 409 分支会直接
// 变成 200（降级门与 WARN 永不可达）。
func TestTemplateExtCodexTransport(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-codex-transport","base_url":"",
		"credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create codex template: %s", rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	extPath := "/api/admin/templates/" + itoa64(tpl.ID) + "/ext"

	// —— 写 ws：200 且**回显** ws（回显恒非 nil——"" 也是有意义的取值）——
	rec = do(http.MethodPut, extPath, `{"credential_type":"codex-oauth","codex_responses_transport":"ws"}`)
	require.Equal(t, 200, rec.Code, "put ws: %s", rec.Body.String())
	var ext TemplateExt
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.NotNil(t, ext.CodexResponsesTransport, "回显恒非 nil（不是「空则省略」）")
	require.Equal(t, "ws", *ext.CodexResponsesTransport, "ws 必须原样落库并回显")

	// GET 同值（读面也走 toAPITemplateExt）
	rec = do(http.MethodGet, extPath, "")
	require.Equal(t, 200, rec.Code, "get ext: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "ws", *ext.CodexResponsesTransport, "GET 回显 ws")

	// —— 降级门：ws → 缺省（= ""）不带 confirm ⇒ 409，且不落库 ——
	rec = do(http.MethodPut, extPath, `{"credential_type":"codex-oauth"}`)
	require.Equal(t, 409, rec.Code, "ws → 缺省且未确认必须 409: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "transport-downgrade", "409 消息须给出确认令牌")
	rec = do(http.MethodGet, extPath, "")
	require.Equal(t, 200, rec.Code, "get ext after 409: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "ws", *ext.CodexResponsesTransport, "409 不得落库")

	// —— 带 confirm 重发同一 body ⇒ 200 且回显 ""（全列替换的「清空」）——
	rec = do(http.MethodPut, extPath+"?confirm=transport-downgrade", `{"credential_type":"codex-oauth"}`)
	require.Equal(t, 200, rec.Code, "confirmed downgrade: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "", *ext.CodexResponsesTransport, "缺省字段 ⇒ 落 \"\"（默认面）并回显")

	// old 已是 ""：显式 ""（不带 confirm）不再 409
	rec = do(http.MethodPut, extPath, `{"credential_type":"codex-oauth","codex_responses_transport":""}`)
	require.Equal(t, 200, rec.Code, "http → \"\": %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "", *ext.CodexResponsesTransport)

	// 显式 http 与 "" 都落库原样（库里能区分这两个值）
	rec = do(http.MethodPut, extPath, `{"credential_type":"codex-oauth","codex_responses_transport":"http"}`)
	require.Equal(t, 200, rec.Code, "explicit http: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "http", *ext.CodexResponsesTransport, "显式 http 原样回显（不归一成 \"\"）")

	// 再写回 ws，供后面的降级门用例使用
	rec = do(http.MethodPut, extPath, `{"credential_type":"codex-oauth","codex_responses_transport":"ws"}`)
	require.Equal(t, 200, rec.Code, "re-arm ws: %s", rec.Body.String())

	// —— 脏值：不做 trim/归一/大小写折叠，原样交给 service 去 400 ——
	for _, dirty := range []string{"WS", "websocket", " "} {
		body := `{"credential_type":"codex-oauth","codex_responses_transport":"` + dirty + `"}`
		rec = do(http.MethodPut, extPath+"?confirm=transport-downgrade", body)
		require.Equal(t, 400, rec.Code, "脏值 %q 必须 400: %s", dirty, rec.Body.String())
	}

	// —— 非 codex：非空值 400（"http" 与 "ws" 一样是永不生效的配置），"" 放行 ——
	rec = do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-custom-transport","base_url":"https://u",
		"credential_type":"custom","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create custom template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	customPath := "/api/admin/templates/" + itoa64(tpl.ID) + "/ext"

	rec = do(http.MethodPut, customPath, `{"credential_type":"custom","codex_responses_transport":"http"}`)
	require.Equal(t, 400, rec.Code, "非 codex + http 必须 400: %s", rec.Body.String())
	rec = do(http.MethodPut, customPath, `{"credential_type":"custom","codex_responses_transport":""}`)
	require.Equal(t, 200, rec.Code, "非 codex + \"\" 放行: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "", *ext.CodexResponsesTransport)

	// —— 陈旧 ext 行幂等例外：模板类型改掉后原样带回旧值 ⇒ 200（不是 400）——
	// 造一条「codex 行上残留 ws、父模板已改成 custom」的真实处境。
	rec = do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-stale-row","base_url":"",
		"credential_type":"codex-pat","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create stale template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	stalePath := "/api/admin/templates/" + itoa64(tpl.ID) + "/ext"
	rec = do(http.MethodPut, stalePath, `{"credential_type":"codex-pat","codex_responses_transport":"ws"}`)
	require.Equal(t, 200, rec.Code, "arm stale row ws: %s", rec.Body.String())

	// 父模板改类型（ext 行不动 ⇒ 留下 codex 行 + ws 的陈旧组合）
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID), `{
		"name":"t-stale-row","base_url":"https://u",
		"credential_type":"custom","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "retype template: %s", rec.Body.String())

	// 第 2 步（类型）**先于**第 3 步（降级门）：新值不等于旧值且非 codex ⇒ 400
	rec = do(http.MethodPut, stalePath, `{"credential_type":"custom","codex_responses_transport":"http"}`)
	require.Equal(t, 400, rec.Code, "陈旧行改值到非 codex 面必须 400（不是 409）: %s", rec.Body.String())

	// 原样带回旧值（ws == old）⇒ 幂等放行 200
	rec = do(http.MethodPut, stalePath, `{"credential_type":"custom","codex_responses_transport":"ws"}`)
	require.Equal(t, 200, rec.Code, "陈旧行原样带回旧值必须 200: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "ws", *ext.CodexResponsesTransport)

	// —— 模板 create/update **不收**该字段（DisallowUnknownFields 的契约收紧）——
	// 字段换了写面：静默忽略会让「配了却不生效」重演，因此旧客户端上送即 400。
	rec = do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-create-transport","base_url":"https://u","supported_formats":["openai-chat"],
		"codex_responses_transport":"ws"}`)
	require.Equal(t, 400, rec.Code, "模板 create 上送传输字段必须 400: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID), `{
		"name":"t-stale-row","base_url":"https://u","supported_formats":["openai-chat"],
		"codex_responses_transport":""}`)
	require.Equal(t, 400, rec.Code, "模板 update 上送传输字段必须 400: %s", rec.Body.String())

	// —— 无 ext 行 ⇒ GET 404（api_key 模板没有 ext 行）——
	rec = do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-key-transport","base_url":"https://u","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create api_key template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodGet, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", "")
	require.Equal(t, 404, rec.Code, "无 ext 行 → 404 保持: %s", rec.Body.String())
}
