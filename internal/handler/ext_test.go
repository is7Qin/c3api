// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTemplatesIdExt 模板 ext 端点：PUT 幂等写入（strip_image_tools 三类型公共
// 能力开关 roundtrip + NULL 清空）+ GET 回显 + 类型一致性 400（special 模板挂
// oauth 行）+ 凭据列拒绝 400（模板 ext 无凭据列——oauth/pat 一律在 account_ext）
// + 父模板缺失 404。
func TestTemplatesIdExt(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// special 模板（只支持 resp 格式）
	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"t-special","base_url":"https://u",
		"credential_type":"responses-special","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create special template: %s", rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.Equal(t, TemplateCredentialTypeResponsesSpecial, *tpl.CredentialType, "credential_type roundtrip")

	// PUT ext（special + strip_image_tools）→ 200 + roundtrip
	rec = do(http.MethodPut, "/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"responses-special","strip_image_tools":true}`)
	require.Equal(t, 200, rec.Code, "put template ext: %s", rec.Body.String())
	var ext TemplateExt
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, TemplateExtCredentialTypeResponsesSpecial, ext.CredentialType)
	require.NotNil(t, ext.StripImageTools)
	require.True(t, *ext.StripImageTools)
	require.Equal(t, tpl.ID, *ext.TemplateId, "响应带 template_id")

	// GET 回显
	rec = do(http.MethodGet, "/admin/templates/"+itoa64(tpl.ID)+"/ext", "")
	require.Equal(t, 200, rec.Code, "get template ext: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.True(t, *ext.StripImageTools)

	// 类型一致性：special 模板挂 oauth 行 → 400（父模板类型不一致）
	rec = do(http.MethodPut, "/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"codex-oauth"}`)
	require.Equal(t, 400, rec.Code, "special 模板 ext 行类型必须一致（oauth 拒绝）: %s", rec.Body.String())

	// 凭据列不再存在：模板 ext 无 oauth/pat 凭据列（一律在 account_ext）——
	// 请求携带 oauth_token 被忽略（契约无该字段），不产生配置
	rec = do(http.MethodPut, "/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"responses-special","oauth_token":"at"}`)
	require.Equal(t, 200, rec.Code, "模板 ext 忽略 oauth_token（契约无凭据列）: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Nil(t, ext.StripImageTools, "携带凭据列不产生配置")

	// oauth 模板 + strip 开关 roundtrip（三类型公共能力）+ 幂等再写（nil 清空）
	rec = do(http.MethodPost, "/admin/templates", `{
		"name":"t-oauth","base_url":"https://u",
		"credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create oauth template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPut, "/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"codex-oauth","strip_image_tools":true}`)
	require.Equal(t, 200, rec.Code, "put template ext oauth strip: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.True(t, *ext.StripImageTools, "oauth 模板 strip 开关可配置")
	rec = do(http.MethodPut, "/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"codex-oauth"}`)
	require.Equal(t, 200, rec.Code, "put template ext nil strip: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Nil(t, ext.StripImageTools, "nil 显式清列（NULL 落库）")

	// api_key 类型模板（无 ext 行）→ PUT 400 / GET 404
	rec = do(http.MethodPost, "/admin/templates", `{
		"name":"t-key","base_url":"https://u","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create api_key template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPut, "/admin/templates/"+itoa64(tpl.ID)+"/ext",
		`{"credential_type":"api_key"}`)
	require.Equal(t, 400, rec.Code, "api_key 类型不允许 ext 行: %s", rec.Body.String())
	rec = do(http.MethodGet, "/admin/templates/"+itoa64(tpl.ID)+"/ext", "")
	require.Equal(t, 404, rec.Code, "无 ext 行 → 404: %s", rec.Body.String())

	// 父模板缺失 → 404
	rec = do(http.MethodPut, "/admin/templates/999999/ext", `{"credential_type":"codex-pat"}`)
	require.Equal(t, 404, rec.Code, "父模板缺失 → 404: %s", rec.Body.String())
}

// TestAccountsIdExt 账号 ext 端点：PUT（oauth/pat 各自父模板同类型行）+ GET
// 回显 + 类型一致性 400（oauth 模板账号挂 pat 行 / api_key 模板账号挂 codex
// 行）+ oauth 最小完整性 400（无 token）+ 类型白名单 400 + 父账号缺失 404。
func TestAccountsIdExt(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"t-codex","base_url":"https://u",
		"credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create codex template: %s", rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	rec = do(http.MethodPost, "/admin/accounts",
		`{"name":"acc-oauth","template_id":`+itoa64(tpl.ID)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var acc Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))

	// PUT oauth ext（身份缺省）→ 200 + service 自动生成四元组（email 非自动生成）
	rec = do(http.MethodPut, "/admin/accounts/"+itoa64(*acc.ID)+"/ext",
		`{"credential_type":"codex-oauth","oauth_token":"at","oauth_refresh_token":"rt"}`)
	require.Equal(t, 200, rec.Code, "put account ext: %s", rec.Body.String())
	var ext AccountExt
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, AccountExtCredentialTypeCodexOauth, ext.CredentialType)
	require.NotNil(t, ext.InstallationId, "首次写入自动生成 installation_id")
	require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, *ext.InstallationId)
	require.NotNil(t, ext.SessionId)
	require.Equal(t, ext.SessionId, ext.ThreadId, "主线程 thread_id == session_id")
	require.Equal(t, *ext.ThreadId+":0", *ext.WindowId, "window_id = {thread_id}:0")
	require.Nil(t, ext.Email, "email 非自动生成（NewCodexIdentity 只生成身份四元组）")
	require.Equal(t, "at", *ext.OauthToken)
	require.Equal(t, *acc.ID, *ext.AccountId)
	autoIID := *ext.InstallationId

	// GET 回显（身份持久复用）
	rec = do(http.MethodGet, "/admin/accounts/"+itoa64(*acc.ID)+"/ext", "")
	require.Equal(t, 200, rec.Code, "get account ext: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "rt", *ext.OauthRefreshToken)
	require.Equal(t, autoIID, *ext.InstallationId)

	// 类型一致性：oauth 模板账号挂 pat 行 → 400（父模板类型不一致）
	rec = do(http.MethodPut, "/admin/accounts/"+itoa64(*acc.ID)+"/ext",
		`{"credential_type":"codex-pat","pat_key":"pat"}`)
	require.Equal(t, 400, rec.Code, "oauth 模板账号 ext 行类型必须一致（pat 拒绝）: %s", rec.Body.String())

	// oauth 最小完整性：无 oauth_token → 400
	rec = do(http.MethodPut, "/admin/accounts/"+itoa64(*acc.ID)+"/ext",
		`{"credential_type":"codex-oauth"}`)
	require.Equal(t, 400, rec.Code, "oauth 行至少 oauth_token: %s", rec.Body.String())

	// pat 模板 + 账号：显式身份 + email（导入时人工/上游填写）→ 采用
	rec = do(http.MethodPost, "/admin/templates", `{
		"name":"t-pat","base_url":"https://u",
		"credential_type":"codex-pat","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create pat template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPost, "/admin/accounts",
		`{"name":"acc-pat","template_id":`+itoa64(tpl.ID)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create pat account: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	rec = do(http.MethodPut, "/admin/accounts/"+itoa64(*acc.ID)+"/ext",
		`{"credential_type":"codex-pat","pat_key":"pat2","email":"user@example.com","session_id":"s1","thread_id":"s1","window_id":"s1:0"}`)
	require.Equal(t, 200, rec.Code, "put account ext explicit identity: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Equal(t, "pat2", *ext.PatKey)
	require.Equal(t, "user@example.com", *ext.Email, "email roundtrip")
	require.Equal(t, "s1", *ext.SessionId)
	require.Equal(t, "s1", *ext.ThreadId, "thread==session 恒等")
	require.Equal(t, "s1:0", *ext.WindowId)

	// 类型一致性：api_key 模板账号挂 codex 行 → 400
	rec = do(http.MethodPost, "/admin/templates", `{
		"name":"t-key","base_url":"https://u","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create api_key template: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPost, "/admin/accounts",
		`{"name":"acc-key","template_id":`+itoa64(tpl.ID)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create api_key account: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	rec = do(http.MethodPut, "/admin/accounts/"+itoa64(*acc.ID)+"/ext",
		`{"credential_type":"codex-oauth","oauth_token":"at"}`)
	require.Equal(t, 400, rec.Code, "api_key 模板账号不允许 codex ext 行: %s", rec.Body.String())

	// 类型白名单：responses-special 账号 ext → 400
	rec = do(http.MethodPut, "/admin/accounts/"+itoa64(*acc.ID)+"/ext",
		`{"credential_type":"responses-special"}`)
	require.Equal(t, 400, rec.Code, "账号 ext 不接受 special: %s", rec.Body.String())

	// 父账号缺失 → 404
	rec = do(http.MethodPut, "/admin/accounts/999999/ext", `{"credential_type":"codex-pat","pat_key":"p"}`)
	require.Equal(t, 404, rec.Code, "父账号缺失 → 404: %s", rec.Body.String())
}

// TestGroupProtocolConvertAPI 分组 protocol_convert 方向集合：创建/更新
// roundtrip（多值数组回显）+ 缺省 = 空数组（off）+ PUT 缺省保持原值/显式空
// 数组清空 + 非法值/同客户端格式冲突 400。
func TestGroupProtocolConvertAPI(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// 缺省 → 空数组（off）
	rec := do(http.MethodPost, "/admin/groups", `{"name":"g-default"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Empty(t, *g.ProtocolConvert, "缺省 protocol_convert = 空数组（off）")

	// 显式多方向 → roundtrip（数组形态回显）
	rec = do(http.MethodPost, "/admin/groups",
		`{"name":"g-multi","protocol_convert":["chat_to_resp","mess_to_resp"]}`)
	require.Equal(t, 200, rec.Code, "create group multi: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, []GroupProtocolConvert{ChatToResp, MessToResp}, *g.ProtocolConvert, "多方向 roundtrip")

	// 更新换方向（显式数组覆盖）→ 生效
	rec = do(http.MethodPut, "/admin/groups/"+itoa64(*g.ID),
		`{"name":"g-multi","protocol_convert":["resp_to_mess"]}`)
	require.Equal(t, 200, rec.Code, "update group: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, []GroupProtocolConvert{RespToMess}, *g.ProtocolConvert)

	// PUT 缺省（省略键）= 保持原值
	rec = do(http.MethodPut, "/admin/groups/"+itoa64(*g.ID), `{"name":"g-multi"}`)
	require.Equal(t, 200, rec.Code, "update group omit: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, []GroupProtocolConvert{RespToMess}, *g.ProtocolConvert, "PUT 缺省保持原值")

	// PUT 显式空数组 = 清空既有方向（off）
	rec = do(http.MethodPut, "/admin/groups/"+itoa64(*g.ID),
		`{"name":"g-multi","protocol_convert":[]}`)
	require.Equal(t, 200, rec.Code, "update group clear: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Empty(t, *g.ProtocolConvert, "显式空数组 = 清空（off）")

	// 非法值 → 400（连字符命名非法，枚举用下划线）
	rec = do(http.MethodPost, "/admin/groups", `{"name":"g-bad","protocol_convert":["chat-to-resp"]}`)
	require.Equal(t, 400, rec.Code, "非法 protocol_convert 必须 400: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/groups/"+itoa64(*g.ID), `{"name":"g-multi","protocol_convert":["bogus"]}`)
	require.Equal(t, 400, rec.Code, "非法 protocol_convert 更新必须 400: %s", rec.Body.String())

	// 同客户端格式冲突（chat_to_resp + chat_to_mess）→ 400
	rec = do(http.MethodPost, "/admin/groups",
		`{"name":"g-clash","protocol_convert":["chat_to_resp","chat_to_mess"]}`)
	require.Equal(t, 400, rec.Code, "同客户端格式多方向必须 400: %s", rec.Body.String())
}

func itoa64(i int64) string {
	return strconv.FormatInt(i, 10)
}
