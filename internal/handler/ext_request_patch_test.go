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

// TestTemplatesIdExtRequestPatch 模板 ext 端点的 request_patch 往返
// （spec custom-request-patch §5.4/§11.1/§11.11）：
//   - PUT custom 带 request_patch → 200；GET 回显**逐字一致**（占位符仍是
//     {api_key}、大整数 9007199254740993 与 1e18 不经 float64）；
//   - PUT 缺省 request_patch ⇒ 清空（旧客户端兼容，与 upstream_paths 同语义）；
//   - 非 custom 模板非空 ⇒ 400（custom-only）；
//   - wire 非法（Accept-Encoding / 未知格式键 / 非法 path / 非法头名）⇒ 400，
//     消息前缀 "invalid request_patch: "；
//   - account ext 端点行为不变（本用例不触它，既有 TestAccountsIdExt 覆盖）。
func TestTemplatesIdExtRequestPatch(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-custom-rp","base_url":"https://u","credential_type":"custom",
		"supported_formats":["openai-chat","anthropic"]}`)
	require.Equal(t, 200, rec.Code, "create custom template: %s", rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	// 大整数 9007199254740993（2^53+1）+ 1e18 字面量 + 嵌套对象 + 空值删除 +
	// 多格式层 + {api_key} 占位符：PUT 后回显必须原样。
	putBody := `{"credential_type":"custom","strip_image_tools":true,
	  "upstream_paths":{"openai-chat":"v1/chat/completions"},
	  "request_patch":{
	    "headers":{"Authorization":"Bearer {api_key}","x-call-source":"vibe_code","X-Drop":""},
	    "formats":{
	      "openai-chat":{"headers":{"User-Agent":"vibe-code/1.0"},
	        "set":{"metadata.call_source":"vibe_code","max_output_tokens":4096,
	               "big":9007199254740993,"e18":1e18,"nested":{"b":2,"a":1}},
	        "delete":["metadata.client_field"]},
	      "anthropic":{"headers":{"Anthropic-Beta":"x"}}
	    }}}`
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", putBody)
	require.Equal(t, 200, rec.Code, "put request_patch: %s", rec.Body.String())
	putResp := rec.Body.String()
	require.Contains(t, putResp, "9007199254740993", "大整数不得被 float64 截断（UseNumber）")
	require.Contains(t, putResp, "1e18", "指数字面量在 handler 层不得被改写")
	require.Contains(t, putResp, "Bearer {api_key}", "占位符原样回显（不替换/不掩码）")
	require.Contains(t, putResp, "v1/chat/completions", "upstream_paths 与新字段并存")

	var ext TemplateExt
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.NotNil(t, ext.RequestPatch)
	require.NotNil(t, ext.RequestPatch.Headers)
	require.Equal(t, "Bearer {api_key}", (*ext.RequestPatch.Headers)["Authorization"])
	require.Equal(t, "", (*ext.RequestPatch.Headers)["X-Drop"], "空值 = Del 条目必须回显（不是丢字段）")
	require.NotNil(t, ext.RequestPatch.Formats)
	require.Contains(t, *ext.RequestPatch.Formats, "anthropic", "多格式层必须逐层回显")

	// GET 回显与 PUT 响应逐字一致（同一规范形；只有请求/响应 envelope 无差异）。
	rec = do(http.MethodGet, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", "")
	require.Equal(t, 200, rec.Code, "get request_patch: %s", rec.Body.String())
	require.Equal(t, putResp, rec.Body.String(), "GET 必须逐字回显 PUT 写入的 request_patch")

	// 旧客户端：不带 request_patch ⇒ 清空（全列替换语义），且其余字段仍可写。
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"custom","upstream_paths":{"openai-chat":"v1/chat/completions"}}`)
	require.Equal(t, 200, rec.Code, "put without request_patch: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"request_patch":null`, "缺省 = 清空 ⇒ 回显 null")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Nil(t, ext.RequestPatch)
	require.NotNil(t, ext.UpstreamPaths, "清空 request_patch 不得连带丢 upstream_paths")

	// 非 custom 模板非空 ⇒ 400（responses-special 有 ext 行）。
	rec = do(http.MethodPost, "/api/admin/templates", `{
		"name":"t-special-rp","base_url":"https://u","credential_type":"responses-special",
		"supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var special Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &special))
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(special.ID)+"/ext",
		`{"credential_type":"responses-special","request_patch":{"headers":{"X-A":"1"}}}`)
	require.Equal(t, 400, rec.Code, "非 custom 非空 request_patch ⇒ 400: %s", rec.Body.String())

	// wire 非法：每条都必须 400 且消息前缀可识别。
	for _, tc := range []struct {
		name string
		body string
	}{
		{"accept-encoding", `{"credential_type":"custom","request_patch":{"headers":{"accept-encoding":"gzip"}}}`},
		{"unknown format key", `{"credential_type":"custom","request_patch":{"formats":{"openai-chatx":{"set":{"a":1}}}}}`},
		{"invalid path", `{"credential_type":"custom","request_patch":{"formats":{"openai-chat":{"set":{"a..b":1}}}}}`},
		{"invalid header name", `{"credential_type":"custom","request_patch":{"headers":{"Bad Name":"v"}}}`},
		{"invalid value", `{"credential_type":"custom","request_patch":{"headers":{"X-A":"a\r\nb"}}}`},
	} {
		rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", tc.body)
		require.Equal(t, 400, rec.Code, tc.name+": "+rec.Body.String())
		require.Contains(t, rec.Body.String(), "invalid request_patch", tc.name)
	}

	// 契约层形状非法（request_patch 不是对象）：在 decode 阶段即 400（invalid json），
	// 到不了 domain 的 wire 校验——同样是拒绝，不是静默忽略。
	rec = do(http.MethodPut, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"custom","request_patch":[1,2]}`)
	require.Equal(t, 400, rec.Code, "request_patch 非对象必须 400: %s", rec.Body.String())

	// 清空后再 GET 仍是 null（400 失败不得半应用）。
	rec = do(http.MethodGet, "/api/admin/templates/"+itoa64(tpl.ID)+"/ext", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Nil(t, ext.RequestPatch, "400 的请求不得部分写入")
}
