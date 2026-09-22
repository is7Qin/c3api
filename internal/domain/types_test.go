// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTemplateFormatSupports(t *testing.T) {
	tpl := &Template{
		SupportedFormats: []RequestFormat{FormatOpenAIChat, FormatAnthropic},
		Models:           []string{"gpt-4o", "claude-3"},
		FormatModels:     map[RequestFormat][]string{FormatOpenAIChat: {"gpt-4o"}},
	}
	require.True(t, tpl.FormatSupports(FormatOpenAIChat, "gpt-4o"))
	require.False(t, tpl.FormatSupports(FormatOpenAIChat, "claude-3"), "配置了格式 → 仅列表内模型")
	require.True(t, tpl.FormatSupports(FormatAnthropic, "gpt-4o"), "未配置格式 → 全部模型")
	require.False(t, tpl.FormatSupports(FormatOpenAIResponses, "gpt-4o"), "格式不在 supported")
	require.True(t, tpl.Serves("gpt-4o"))
	require.False(t, tpl.Serves("nonexistent"))
	require.Equal(t, []RequestFormat{FormatOpenAIChat, FormatAnthropic}, tpl.FormatsFor())
}

func TestTemplateServes(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		FormatModels: map[RequestFormat][]string{FormatOpenAIResponses: {"o3"}},
		ModelMapping: ModelMapping{"claude-sonnet": {MappedModel: "claude-sonnet-4-5", Mode: ModelMappingModeExplicit}},
	}
	require.True(t, tpl.Serves("gpt-4o"), "serves models")
	require.True(t, tpl.Serves("o3"), "serves format_models list values")
	require.True(t, tpl.Serves("claude-sonnet"), "serves mapping keys")
	require.False(t, tpl.Serves("nope"))
}

func TestRequestFormatValid(t *testing.T) {
	for _, f := range []RequestFormat{
		FormatOpenAIChat, FormatOpenAIResponses, FormatOpenAIResponsesWS, FormatAnthropic,
		FormatOpenAIImages, // spec §4.3：openai-images（images 端点落库 format）
		FormatOpenAISearch, // spec 2026-08-13：openai-search（search 端点落库 format——本 task 只扩枚举）
	} {
		require.True(t, f.Valid(), "format %s should be valid", f)
	}
	require.False(t, RequestFormat("gemini").Valid())
	require.False(t, RequestFormat("openai-images-extra").Valid())
}

func TestTruncateErrMsg(t *testing.T) {
	// 短文本原样返回（零分配路径；全部 ASCII 错误文案 < 500 字符）
	require.Equal(t, "", TruncateErrMsg(""))
	require.Equal(t, "boom", TruncateErrMsg("boom"))
	require.Equal(t, strings.Repeat("a", ErrMsgMaxLen), TruncateErrMsg(strings.Repeat("a", ErrMsgMaxLen)))
	// 超限按 500 字符截断
	require.Equal(t, strings.Repeat("a", ErrMsgMaxLen), TruncateErrMsg(strings.Repeat("a", 600)))
	// 多字节 UTF-8 不拆断：600 个「界」= 1800 字节 → 截 500 字符（1500 字节）
	got := TruncateErrMsg(strings.Repeat("界", 600))
	require.Equal(t, 500, utf8.RuneCountInString(got))
	require.True(t, utf8.ValidString(got), "截断不得产生非法 UTF-8")
	// 字节超限但字符数未超限 → 原样返回
	require.Equal(t, strings.Repeat("界", 300), TruncateErrMsg(strings.Repeat("界", 300)))
}

// —— intelligent-routing cutover 领域契约锁（中性结构断言：只枚举现存契约面，
// 契约面变更 = 显式改白名单，防止运行时调度态重新渗入持久/API 实体）——

// exportedFieldNames 导出字段名升序集合（结构契约枚举用）。
func exportedFieldNames(rt reflect.Type) []string {
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); f.IsExported() {
			names = append(names, f.Name)
		}
	}
	slices.Sort(names)
	return names
}

func TestAccountDomainContractFieldSet(t *testing.T) {
	// 账号实体 = 持久/API 契约面；运行时调度态一律由 scheduler 内存视图承载，
	// 不进本结构。
	require.Equal(t, []string{
		"BaseURL", "CacheDomain", "CreatedAt", "DeletedAt", "Enabled", "Ext",
		"FailedAt", "FailureSource", "GroupIDs", "ID", "IdentityRevision", "LastError",
		"LastUsedAt", "LifecycleRevision", "MaxConcurrency", "Name", "Template", "TemplateID",
		"UpdatedAt", "UpstreamCostMultiplierBp", "UpstreamKey",
	}, exportedFieldNames(reflect.TypeOf(Account{})))
}

func TestCodexImportItemContractFieldSets(t *testing.T) {
	// 批量导入行 = 配置面（max_concurrency）+ 凭据/身份，无调度旋钮。
	require.Equal(t, []string{
		"CodexAccountID", "CodexEmail", "CodexOAuthExpiresAt",
		"CodexOAuthRefreshToken", "CodexOAuthToken", "MaxConcurrency",
	}, exportedFieldNames(reflect.TypeOf(CodexOAuthImportItem{})))
	require.Equal(t, []string{
		"CodexAccountID", "CodexEmail", "CodexPATKey", "MaxConcurrency",
	}, exportedFieldNames(reflect.TypeOf(CodexPATImportItem{})))
}

func TestAccountStatusIsRuntimeOnlyType(t *testing.T) {
	// AccountStatus 只允许作为内存调度视图（RuntimeInfo 等）的字段类型；
	// 账号持久/API 契约结构与规则动作契约一律不得携带该类型字段。
	statusT := reflect.TypeOf(AccountStatus(""))
	statusPT := reflect.TypeOf((*AccountStatus)(nil))
	for _, v := range []any{Account{}, RuleThen{}, CodexOAuthImportItem{}, CodexPATImportItem{}} {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			require.NotEqual(t, statusT, f.Type, "%s.%s", rt.Name(), f.Name)
			require.NotEqual(t, statusPT, f.Type, "%s.%s", rt.Name(), f.Name)
		}
	}
}

func TestRuleThenJSONContractShape(t *testing.T) {
	// then 的 JSON 表示（API then 对象 = then_json 落库形态）键集即契约全集；
	// 全字段填充后 marshal 不得多键、round-trip 不得丢键。
	rc := 502
	msg := "upstream rejected request"
	dur := int64(1000)
	full := RuleThen{
		ResponseCode:  &rc,
		CustomMessage: &msg,
		Throttle: &ThrottleAction{
			Scope: ThrottleScopeAccount, Mode: ThrottleModeOpen, DurationMs: &dur,
		},
		FailAccount: true,
	}
	b, err := json.Marshal(full)
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &keys))
	keyNames := make([]string, 0, len(keys))
	for k := range keys {
		keyNames = append(keyNames, k)
	}
	slices.Sort(keyNames)
	require.Equal(t, []string{"custom_message", "fail_account", "response_code", "throttle"}, keyNames)
	var back RuleThen
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, full, back)
}

func TestRuleThenUnknownKeyHasNoLandingSite(t *testing.T) {
	// 域表示无法保留契约外键（写入面的严格拒绝由 service 边界
	// DisallowUnknownFields 承载；此处锁域结构本身无落点）。
	var got RuleThen
	require.NoError(t, json.Unmarshal([]byte(`{"response_code":502,"unexpected_key":"x"}`), &got))
	rc := 502
	require.Equal(t, RuleThen{ResponseCode: &rc}, got)
}
