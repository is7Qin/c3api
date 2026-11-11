// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestResolveUpstreamPath A4：路径键/默认端点/自定义覆盖的单一实现。proxy 的
// targetPath 与探测执行都委托到 domain，行为必须与既有 proxy 用例逐字节一致。
func TestResolveUpstreamPath(t *testing.T) {
	cases := []struct {
		key  domain.UpstreamPathKey
		tail string
	}{
		{domain.UpstreamPathChat, "chat/completions"},
		{domain.UpstreamPathResponses, "responses"},
		{domain.UpstreamPathResponsesWS, "responses"},
		{domain.UpstreamPathAnthropic, "messages"},
		{domain.UpstreamPathSearch, "alpha/search"},
		{domain.UpstreamPathImagesGen, "images/generations"},
		{domain.UpstreamPathImagesEdits, "images/edits"},
	}
	require.Len(t, cases, 7, "7 个键必须全部覆盖（新增键时同步此表）")
	for _, tc := range cases {
		require.Equal(t, tc.tail, domain.UpstreamDefaultTail(tc.key))
		require.False(t, len(tc.tail) > 2 && tc.tail[:3] == "v1/", "默认尾段不得含 v1 协议段")
		require.NotEqual(t, '/', tc.tail[0], "默认尾段不得带前导斜杠")

		// 非 custom：即使 map 里残留配置也回落内置端点。
		require.Equal(t, "/v1/"+tc.tail,
			domain.ResolveUpstreamPath(credential.TypeAPIKey, map[string]string{string(tc.key): "v1/x"}, tc.key, tc.tail),
			"非 custom 必须忽略自定义路径")
		require.Equal(t, "/v1/"+tc.tail,
			domain.ResolveUpstreamPath(credential.TypeCustom, map[string]string{string(tc.key): ""}, tc.key, tc.tail),
			"空值等同未配置")
		// custom：原样透传（不补 /v1、不改写）。
		require.Equal(t, "v1/x",
			domain.ResolveUpstreamPath(credential.TypeCustom, map[string]string{string(tc.key): "v1/x"}, tc.key, tc.tail))
	}

	for key, want := range map[string]domain.RequestFormat{
		"openai-chat":               domain.FormatOpenAIChat,
		"openai-responses":          domain.FormatOpenAIResponses,
		"openai-responses-ws":       domain.FormatOpenAIResponsesWS,
		"anthropic":                 domain.FormatAnthropic,
		"openai-search":             domain.FormatOpenAISearch,
		"openai-images.generations": domain.FormatOpenAIImages,
		"openai-images.edits":       domain.FormatOpenAIImages,
	} {
		got, ok := domain.UpstreamPathFormat(key)
		require.True(t, ok, "键 %q 必须属于固定全集", key)
		require.Equal(t, want, got)
	}
	_, ok := domain.UpstreamPathFormat("unknown-key")
	require.False(t, ok)

	// 探测可用的格式 → 路径键映射；images/search 不参与探测。
	for _, tc := range []struct {
		f    domain.RequestFormat
		key  domain.UpstreamPathKey
		used bool
	}{
		{domain.FormatOpenAIChat, domain.UpstreamPathChat, true},
		{domain.FormatOpenAIResponses, domain.UpstreamPathResponses, true},
		{domain.FormatOpenAIResponsesWS, domain.UpstreamPathResponsesWS, true},
		{domain.FormatAnthropic, domain.UpstreamPathAnthropic, true},
		{domain.FormatOpenAIImages, "", false},
		{domain.FormatOpenAISearch, "", false},
	} {
		key, used := domain.UpstreamPathKeyForFormat(tc.f)
		require.Equal(t, tc.used, used)
		require.Equal(t, tc.key, key)
	}
}

// TestMapModel A18：映射命中规则的单一实现（调度两个活跃读点与探测共用）。
func TestMapModel(t *testing.T) {
	tpl := &domain.Template{ModelMapping: domain.ModelMapping{
		"client": {MappedModel: "upstream", Mode: domain.ModelMappingModeExplicit},
		"same":   {MappedModel: "same", Mode: domain.ModelMappingModeImplicit},
		"empty":  {MappedModel: "", Mode: domain.ModelMappingModeExplicit},
	}}
	require.Equal(t, "upstream", domain.MapModel(tpl, "client"))
	require.Equal(t, "same", domain.MapModel(tpl, "same"))
	require.Equal(t, "", domain.MapModel(tpl, "empty"), "命中即用条目原值（与就地读取一致）")
	require.Equal(t, "unknown", domain.MapModel(tpl, "unknown"))
	require.Equal(t, "unknown", domain.MapModel(nil, "unknown"))

	mapped, mode := domain.MapModelEntry(tpl, "client")
	require.Equal(t, "upstream", mapped)
	require.Equal(t, domain.ModelMappingModeExplicit, mode)
	mapped, mode = domain.MapModelEntry(tpl, "unknown")
	require.Equal(t, "unknown", mapped)
	require.Equal(t, domain.ModelMappingModeInvalid, mode)
}

// TestProxyEgressSpec 下沉后的出口规格派生：nil = 零值（直连），字段原样进
// egress.New 的规范化。
func TestProxyEgressSpec(t *testing.T) {
	var nilProxy *domain.Proxy
	var zero domain.Proxy
	require.Equal(t, zero.EgressSpec(), nilProxy.EgressSpec())

	p := &domain.Proxy{Scheme: domain.ProxySchemeHTTP, Host: "127.0.0.1", Port: 3128, Username: "u", Password: "p"}
	spec := p.EgressSpec()
	require.Equal(t, "http", spec.Scheme)
	require.Equal(t, "127.0.0.1", spec.Host)
	require.Equal(t, 3128, spec.Port)
	require.Equal(t, "u", spec.Username)
	require.Equal(t, "p", spec.Password)
}
