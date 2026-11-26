// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

// request_patch probe 数据面（spec custom-request-patch §7.3/§7.4/§10 P5）：
//   - 5 个 factory 调用（probe.go 3 + content.go 2）全部带装饰器、body patch 在既有
//     载荷合成之后出站之前应用；
//   - 格式键与 URL 路径同一把键（chat/responses/responses-ws/anthropic/search/
//     images.generations/images.edits 七键），images edits 的 multipart 只注入头；
//   - 头语义（两层 / Del / Host 三态含小写 host / 运行期跳 Accept-Encoding /
//     {api_key} 用 spec.Credential 原值）与 proxy 同一份 domain 判据；
//   - body set/delete/set 胜/dirty 残余失败裸透传；
//   - 无配置 helpers 零分配；probe 既有不变量（origin 锁定 / 不跟随重定向 /
//     固定分类）不因 patch 改变。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// probePatchUpstream 把解析后的 patch 装进 ProbeSpec.Upstream（唯一装配入口
// domain.NewTemplateUpstream：空 ⇒ nil）。
func probePatchUpstream(t *testing.T, patch *domain.RequestPatch) *domain.TemplateUpstream {
	t.Helper()
	up := domain.NewTemplateUpstream(nil, patch)
	require.NotNil(t, up, "用例必须真正配置 patch（nil 则用例空转）")
	return up
}

// mustProbePatch 用**保存期唯一入口** ParseRequestPatch 解析 wire JSON（头名规范形、
// 值校验、格式键词汇都在这里走真实路径）。
// mustProbeJSON 把测试夹具对象编码成 wire JSON（避免手写括号层级错误）。
func mustProbeJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

func mustProbePatch(t *testing.T, raw string) *domain.RequestPatch {
	t.Helper()
	patch, err := domain.ParseRequestPatch(json.RawMessage(raw))
	require.NoError(t, err)
	require.NotNil(t, patch, "用例必须真正配置 patch（nil 则用例空转）")
	return patch
}

// customProbeSpec 是带 custom 凭据的探测 spec（request_patch 只出现在 custom 模板；
// 数据面无类型判断，但夹具保持与生产同形）。
func customProbeSpec(format domain.RequestFormat) domain.ProbeSpec {
	spec := contentSpec(format)
	spec.CredentialType = credential.TypeCustom
	spec.Credential = "sk-probe"
	return spec
}

// probeCapture 捕获一次上游请求（自定义 RoundTripper 收到的最终 wire 形态）。
type probeCapture struct {
	host        string
	header      http.Header
	body        []byte
	contentType string
	path        string
}

// captureProbeCall 返回一个把请求收进 cap 的 contentRT（同一探针只应命中一次）。
func captureProbeCall(cap *probeCapture, status int, respBody string) contentRT {
	return func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		cap.host = req.Host
		cap.header = req.Header.Clone()
		cap.body = raw
		cap.contentType = req.Header.Get("Content-Type")
		cap.path = req.URL.Path
		return contentResponse(status, respBody), nil
	}
}

// probeRequestPatchSuccessBody 每个格式的 200 成功判定用最小可用响应体。
func probeRequestPatchSuccessBody(format domain.RequestFormat) string {
	switch format {
	case domain.FormatOpenAIChat:
		return "{\"choices\":[{\"message\":{\"content\":\"hi\"}}]}"
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		return "{\"output\":[{\"type\":\"message\"}]}"
	case domain.FormatAnthropic:
		return "{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}"
	case domain.FormatOpenAISearch:
		return "{\"output\":\"ok\"}"
	default:
		return imagesBody(b64(pngBytes(16)))
	}
}

// TestProbeRequestPatchFormatKeys 七个格式键与 URL 路径**同一把键**（spec §7.3
// 定稿）：每个出站格式只应用自己键下的格式层；responses-ws 用
// openai-responses-ws（不是 openai-responses）；images 按 ImageMode 分
// generations/edits；edits 的 multipart 体结构性跳过（只头）。
func TestProbeRequestPatchFormatKeys(t *testing.T) {
	imagesGen := func() domain.ProbeSpec {
		return contentImageSpec(domain.ProbeImageModeGenerations, "")
	}
	imagesEdits := func() domain.ProbeSpec {
		image := pngBytes(64)
		return contentImageSpec(domain.ProbeImageModeEdits, "data:image/png;base64,"+b64(image))
	}
	cases := []struct {
		name     string
		format   domain.RequestFormat
		layerKey domain.UpstreamPathKey
		spec     func() domain.ProbeSpec
		wantPath string
	}{
		{"chat", domain.FormatOpenAIChat, domain.UpstreamPathChat, func() domain.ProbeSpec { return customProbeSpec(domain.FormatOpenAIChat) }, "/v1/chat/completions"},
		{"responses", domain.FormatOpenAIResponses, domain.UpstreamPathResponses, func() domain.ProbeSpec { return customProbeSpec(domain.FormatOpenAIResponses) }, "/v1/responses"},
		{"responses-ws", domain.FormatOpenAIResponsesWS, domain.UpstreamPathResponsesWS, func() domain.ProbeSpec { return customProbeSpec(domain.FormatOpenAIResponsesWS) }, "/v1/responses"},
		{"anthropic", domain.FormatAnthropic, domain.UpstreamPathAnthropic, func() domain.ProbeSpec { return customProbeSpec(domain.FormatAnthropic) }, "/v1/messages"},
		{"search", domain.FormatOpenAISearch, domain.UpstreamPathSearch, contentSearchSpec, "/v1/alpha/search"},
		{"images-generations", domain.FormatOpenAIImages, domain.UpstreamPathImagesGen, imagesGen, "/v1/images/generations"},
		{"images-edits", domain.FormatOpenAIImages, domain.UpstreamPathImagesEdits, imagesEdits, "/v1/images/edits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer := string(tc.layerKey)
			raw := "{\"headers\":{\"X-Global\":\"g\"},\"formats\":{\"" + layer + "\":{\"headers\":{\"X-Fmt-Key\":\"" + layer + "\"},\"set\":{\"patch_fmt\":\"" + layer + "\"}}}}"
			// responses ws 的探测实际走 HTTP responses 语义：同模板若另有
			// openai-responses 键，必须**不**命中（键与 path 同一把，不做双层叠加）。
			if tc.layerKey == domain.UpstreamPathResponsesWS {
				raw = mustProbeJSON(t, map[string]any{
					"headers": map[string]string{"X-Global": "g"},
					"formats": map[string]any{
						"openai-responses": map[string]any{
							"headers": map[string]string{"X-Wrong-Key": "nope"},
							"set":     map[string]string{"wrong_fmt": "nope"},
						},
						"openai-responses-ws": map[string]any{
							"headers": map[string]string{"X-Fmt-Key": "openai-responses-ws"},
							"set":     map[string]string{"patch_fmt": "openai-responses-ws"},
						},
					},
				})
			}
			spec := tc.spec()
			spec.Upstream = probePatchUpstream(t, mustProbePatch(t, raw))
			var cap probeCapture
			res := newTestProber(t, captureProbeCall(&cap, http.StatusOK, probeRequestPatchSuccessBody(tc.format))).
				Probe(context.Background(), spec)
			require.True(t, res.Success, "error=%s detail=%s", res.Error, res.Detail)

			// 路径不因 patch 变化（键只选格式层，不参与 URL 解析）。
			require.Equal(t, tc.wantPath, cap.path, "patch 不得改变出站路径")
			require.Equal(t, "g", cap.header.Get("X-Global"), "全局层头必须应用")
			require.Equal(t, layer, cap.header.Get("X-Fmt-Key"), "格式层必须命中该出站格式的键（%s）", layer)
			if tc.layerKey == domain.UpstreamPathResponsesWS {
				require.Empty(t, cap.header.Get("X-Wrong-Key"), "responses-ws 探测不得消费 openai-responses 键的头")
				require.Empty(t, gjson.GetBytes(cap.body, "wrong_fmt").String(), "responses-ws 探测不得消费 openai-responses 键的 body patch")
			}

			if tc.layerKey == domain.UpstreamPathImagesEdits {
				require.True(t, strings.HasPrefix(cap.contentType, "multipart/form-data;"), "edits 是 multipart 体：%q", cap.contentType)
				require.NotContains(t, string(cap.body), "patch_fmt", "edits multipart 体必须结构性跳过 body patch（只注入头）")
				require.Contains(t, string(cap.body), "name=\"image\"")
				return
			}
			require.Equal(t, layer, gjson.GetBytes(cap.body, "patch_fmt").String(), "body patch 必须命中该出站格式的键")
		})
	}
}

// TestProbeRequestPatchHeaders 头语义（与 proxy 同一份 domain 判据）：
// 两层覆盖、Del、Host 三态（含小写 host 归一）、运行期脏库 Accept-Encoding 跳过、
// {api_key} 用 spec.Credential 原值且换凭据换值。
func TestProbeRequestPatchHeaders(t *testing.T) {
	newSpec := func(t *testing.T, raw string) domain.ProbeSpec {
		t.Helper()
		spec := customProbeSpec(domain.FormatOpenAIChat)
		spec.Upstream = probePatchUpstream(t, mustProbePatch(t, raw))
		return spec
	}
	run := func(t *testing.T, spec domain.ProbeSpec) probeCapture {
		t.Helper()
		var cap probeCapture
		res := newTestProber(t, captureProbeCall(&cap, http.StatusOK, probeRequestPatchSuccessBody(domain.FormatOpenAIChat))).
			Probe(context.Background(), spec)
		require.True(t, res.Success, "error=%s", res.Error)
		return cap
	}

	t.Run("two_layers_format_overrides_global", func(t *testing.T) {
		cap := run(t, newSpec(t, "{\"headers\":{\"X-Layer\":\"global\",\"X-Only-Global\":\"g\"},\"formats\":{\"openai-chat\":{\"headers\":{\"X-Layer\":\"format\",\"X-Only-Format\":\"f\"}}}}"))
		require.Equal(t, "format", cap.header.Get("X-Layer"), "格式层逐键覆盖全局层")
		require.Equal(t, "g", cap.header.Get("X-Only-Global"))
		require.Equal(t, "f", cap.header.Get("X-Only-Format"))
	})

	t.Run("del_gateway_default_and_cross_layer", func(t *testing.T) {
		// 空值 = Del：全局层先 Set Authorization；格式层空值删它并 Del 网关默认头。
		cap := run(t, newSpec(t, "{\"headers\":{\"Authorization\":\"Bearer {api_key}\",\"X-Layer\":\"global\"},\"formats\":{\"openai-chat\":{\"headers\":{\"X-Layer\":\"\",\"Anthropic-Version\":\"\"}}}}"))
		require.Equal(t, "Bearer sk-probe", cap.header.Get("Authorization"), "全局层先 Set")
		require.Empty(t, cap.header.Get("X-Layer"), "格式层空值 Del 覆盖全局层 Set")
		require.Empty(t, cap.header.Get("Anthropic-Version"), "格式层 Del 可删网关默认头")
		// 完全删除 Authorization（空值落在全局层）。
		cap = run(t, newSpec(t, "{\"headers\":{\"Authorization\":\"\"}}"))
		require.Empty(t, cap.header.Get("Authorization"), "空值 Del 删除网关默认 Authorization")
	})

	t.Run("host_tri_state", func(t *testing.T) {
		cap := run(t, newSpec(t, "{\"headers\":{\"host\":\"virtual.example\"}}"))
		require.Equal(t, "virtual.example", cap.host, "小写 host 归一后覆盖 req.Host")
		require.Empty(t, cap.header.Get("Host"), "Host 特例只写 req.Host，不写 header map")

		// 格式层 Host 空值清除全局层 Set ⇒ 回落 URL.Host（自定义 RoundTripper 看到空 host）。
		cap = run(t, newSpec(t, "{\"headers\":{\"Host\":\"global.example\"},\"formats\":{\"openai-chat\":{\"headers\":{\"Host\":\"\"}}}}"))
		require.Equal(t, "", cap.host, "格式层 Host 空值覆盖全局层 Set（回退出站 URL.Host）")

		// 仅格式层 Set Host，且值里带 {api_key}。
		cap = run(t, newSpec(t, "{\"formats\":{\"openai-chat\":{\"headers\":{\"Host\":\"fmt-{api_key}.example\"}}}}"))
		require.Equal(t, "fmt-sk-probe.example", cap.host, "Host 值里的 {api_key} 同样展开")
	})

	t.Run("dirty_accept_encoding_skipped", func(t *testing.T) {
		// 脏库值：保存期本应 400（非空 AE 硬拒）；这里手工构造绕过写面。
		dirty := &domain.RequestPatch{Headers: map[string]string{
			"Accept-Encoding":  "gzip",
			"X-Dirty-Neighbor": "applied",
		}}
		spec := customProbeSpec(domain.FormatOpenAIChat)
		spec.Upstream = probePatchUpstream(t, dirty)
		cap := run(t, spec)
		require.Empty(t, cap.header.Get("Accept-Encoding"), "运行期永不应用 Accept-Encoding（响应编码不变量）")
		require.Equal(t, "applied", cap.header.Get("X-Dirty-Neighbor"), "同层合法头照常应用")
	})

	t.Run("api_key_from_spec_credential", func(t *testing.T) {
		spec := newSpec(t, "{\"headers\":{\"X-Vendor-Key\":\"{api_key}\",\"Authorization\":\"Bearer {api_key}\"},\"formats\":{\"openai-chat\":{\"headers\":{\"X-Formatted-Key\":\"v={api_key}\"}}}}")
		spec.Credential = "sk-probe-first"
		cap := run(t, spec)
		require.Equal(t, "sk-probe-first", cap.header.Get("X-Vendor-Key"), "{api_key} 用 spec.Credential 原始裸 key 展开")
		require.Equal(t, "Bearer sk-probe-first", cap.header.Get("Authorization"))
		require.Equal(t, "v=sk-probe-first", cap.header.Get("X-Formatted-Key"))

		spec.Credential = "sk-probe-second"
		cap = run(t, spec)
		require.Equal(t, "sk-probe-second", cap.header.Get("X-Vendor-Key"), "换探测凭据 ⇒ 头值随之变化")
		require.NotEqual(t, "sk-probe-first", cap.header.Get("Authorization"))
	})

	t.Run("no_config_unchanged", func(t *testing.T) {
		base := customProbeSpec(domain.FormatOpenAIChat)
		var nilCap probeCapture
		res := newTestProber(t, captureProbeCall(&nilCap, http.StatusOK, probeRequestPatchSuccessBody(domain.FormatOpenAIChat))).
			Probe(context.Background(), base)
		require.True(t, res.Success)

		// 只有 Paths 的 Upstream（无 patch）= 未配置语义 ⇒ wire 逐字节一致。
		pathsOnly := base
		pathsOnly.Upstream = domain.NewTemplateUpstream(map[string]string{}, nil)
		require.Nil(t, pathsOnly.Upstream, "空路径 + 无 patch ⇒ 未配置（唯一归一入口）")
		var pathCap probeCapture
		res = newTestProber(t, captureProbeCall(&pathCap, http.StatusOK, probeRequestPatchSuccessBody(domain.FormatOpenAIChat))).
			Probe(context.Background(), pathsOnly)
		require.True(t, res.Success)
		require.Equal(t, nilCap.header, pathCap.header, "无配置时出站头必须与未引入本特性逐字节一致")
		require.Equal(t, nilCap.body, pathCap.body, "无配置时出站 body 必须逐字节一致")
	})
}

// TestProbeRequestPatchBodySemantics body 语义：set/delete 排序、set 胜、非 JSON
// 结构性跳过、dirty 残余失败裸透传（不中止探测）。
func TestProbeRequestPatchBodySemantics(t *testing.T) {
	runChat := func(t *testing.T, upstream *domain.TemplateUpstream) (domain.ProbeResult, probeCapture) {
		t.Helper()
		spec := customProbeSpec(domain.FormatOpenAIChat)
		spec.Upstream = upstream
		var cap probeCapture
		res := newTestProber(t, captureProbeCall(&cap, http.StatusOK, probeRequestPatchSuccessBody(domain.FormatOpenAIChat))).
			Probe(context.Background(), spec)
		return res, cap
	}

	t.Run("set_delete_set_wins", func(t *testing.T) {
		patch := mustProbePatch(t, "{\"formats\":{\"openai-chat\":{\"delete\":[\"max_tokens\",\"messages.0.content\",\"missing.path\"],\"set\":{\"model\":\"patched-model\",\"stream\":true,\"messages.0.content\":\"set-wins\",\"metadata.call_source\":\"probe\"}}}}")
		res, cap := runChat(t, probePatchUpstream(t, patch))
		require.True(t, res.Success)
		require.Equal(t, "patched-model", gjson.GetBytes(cap.body, "model").String(), "body set 可覆盖既有载荷字段")
		require.False(t, gjson.GetBytes(cap.body, "max_tokens").Exists(), "delete 生效")
		require.False(t, gjson.GetBytes(cap.body, "missing.path").Exists(), "delete 不存在的 path 不是失败")
		require.Equal(t, "set-wins", gjson.GetBytes(cap.body, "messages.0.content").String(), "同一 path 同时 delete+set ⇒ set 胜")
		require.True(t, gjson.GetBytes(cap.body, "stream").Bool())
		require.Equal(t, "probe", gjson.GetBytes(cap.body, "metadata.call_source").String())
	})

	t.Run("images_generations_json_body_patched", func(t *testing.T) {
		patch := mustProbePatch(t, "{\"formats\":{\"openai-images.generations\":{\"set\":{\"response_format\":\"url\"},\"delete\":[\"prompt\"]}}}")
		spec := contentImageSpec(domain.ProbeImageModeGenerations, "")
		spec.Upstream = probePatchUpstream(t, patch)
		var cap probeCapture
		res := newTestProber(t, captureProbeCall(&cap, http.StatusOK, imagesBody(b64(pngBytes(16))))).
			Probe(context.Background(), spec)
		require.True(t, res.Success, "error=%s", res.Error)
		require.Equal(t, "url", gjson.GetBytes(cap.body, "response_format").String(), "images generations 的 JSON 合成体照常 patch")
		require.False(t, gjson.GetBytes(cap.body, "prompt").Exists())
	})

	t.Run("search_body_patched", func(t *testing.T) {
		patch := mustProbePatch(t, "{\"formats\":{\"openai-search\":{\"set\":{\"max_output_tokens\":7,\"metadata.source\":\"probe\"},\"delete\":[\"commands\"]}}}")
		spec := contentSearchSpec()
		spec.Upstream = probePatchUpstream(t, patch)
		var cap probeCapture
		res := newTestProber(t, captureProbeCall(&cap, http.StatusOK, "{\"output\":\"ok\"}")).
			Probe(context.Background(), spec)
		require.True(t, res.Success, "error=%s", res.Error)
		require.Equal(t, int64(7), gjson.GetBytes(cap.body, "max_output_tokens").Int())
		require.Equal(t, "probe", gjson.GetBytes(cap.body, "metadata.source").String())
		require.False(t, gjson.GetBytes(cap.body, "commands").Exists())
	})

	t.Run("multipart_structurally_skipped", func(t *testing.T) {
		// 直接对 probeBodyPatch 断言：multipart（非 JSON 容器）原样返回 ——
		// 设计性跳过，不是失败（也不 WARN）。
		patch := mustProbePatch(t, "{\"formats\":{\"openai-images.edits\":{\"set\":{\"prompt\":\"patched\"}}}}")
		spec := contentImageSpec(domain.ProbeImageModeEdits, "data:image/png;base64,"+b64(pngBytes(16)))
		spec.Upstream = probePatchUpstream(t, patch)
		multipartBody := []byte("--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--b--\r\n")
		got := probeBodyPatch(spec, domain.UpstreamPathImagesEdits, multipartBody)
		require.True(t, bytes.Equal(multipartBody, got), "非 JSON 容器必须原样返回（结构性跳过）")
	})

	t.Run("dirty_residual_failure_forwards_original", func(t *testing.T) {
		// 手工构造脏 patch：x.999999999 超出 sjson 数组 padding 上限，运行期执行失败
		// （保存期本应 400）。语义：裸透传 + 不中止 + 头装饰独立生效。
		patch := &domain.RequestPatch{
			Headers: map[string]string{"X-Patch-Header": "applied"},
			Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
				domain.UpstreamPathChat: {
					Set: map[string]json.RawMessage{
						"x.999999999":   json.RawMessage("1"),
						"secret_marker": json.RawMessage("\"must-not-leak\""),
					},
				},
			},
		}
		res, cap := runChat(t, probePatchUpstream(t, patch))
		require.True(t, res.Success, "残余失败不得中止/改变探测结论：error=%s", res.Error)
		require.Equal(t, "applied", cap.header.Get("X-Patch-Header"), "头装饰与 body 失败相互独立")
		want := staticPayload(domain.FormatOpenAIChat, "up-model", domain.ProbeDefaultInputText)
		require.True(t, bytes.Equal(want, cap.body), "残余失败 ⇒ 裸透传（body 逐字节不变）\n got=%s", cap.body)
		require.NotContains(t, string(cap.body), "secret_marker")
	})
}

// TestProbeRequestPatchHelpersAllocs 无配置零分配硬断言 + 有配置每 probe 至多 1 枚
// 闭包（spec §7.1/§7.3 的成本契约；热路径无日志、无锁、无全局查询）。
func TestProbeRequestPatchHelpersAllocs(t *testing.T) {
	body := []byte("{\"model\":\"gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")

	// 1) 无配置：装饰器 nil + body 原 slice，0 分配。
	allocs := testing.AllocsPerRun(100, func() {
		spec := domain.ProbeSpec{}
		if dec := probeHeaderDecorator(spec, domain.UpstreamPathChat); dec != nil {
			t.Fatalf("无配置必须返回 nil 装饰器")
		}
		if got := probeBodyPatch(spec, domain.UpstreamPathChat, body); &got[0] != &body[0] {
			t.Fatalf("无配置必须返回原 slice")
		}
	})
	require.Zero(t, allocs, "无配置 probe 热路径必须零分配（闭包/切片/解析都不许有）")

	// 2) 配置存在但本格式无层：body 面零分配（只做一次 map 查）。
	otherFormat := customProbeSpec(domain.FormatOpenAIChat)
	otherFormat.Upstream = probePatchUpstream(t, mustProbePatch(t, "{\"formats\":{\"openai-responses\":{\"set\":{\"model\":\"x\"}}}}"))
	allocs = testing.AllocsPerRun(100, func() {
		if got := probeBodyPatch(otherFormat, domain.UpstreamPathChat, body); &got[0] != &body[0] {
			t.Fatalf("本格式无 patch 键 ⇒ 原 slice")
		}
	})
	require.Zero(t, allocs, "配置存在但本格式无 body patch ⇒ 零分配")

	// 3) 有头配置：每 probe 至多 1 枚闭包（apply 阶段的 Header.Set 分配属
	//    net/http 既有语义，不计入本预算；与 proxy 同口径）。
	headerOnly := customProbeSpec(domain.FormatOpenAIChat)
	headerOnly.Upstream = probePatchUpstream(t, mustProbePatch(t, "{\"headers\":{\"X-A\":\"1\"}}"))
	var sink func(*http.Request)
	allocs = testing.AllocsPerRun(100, func() {
		sink = probeHeaderDecorator(headerOnly, domain.UpstreamPathChat)
		if sink == nil {
			t.Fatalf("有头配置必须返回非 nil 装饰器")
		}
	})
	require.LessOrEqual(t, allocs, 1.0, "有配置但无占位符：每 probe ≤1 个闭包分配")
	require.NotNil(t, sink)
}

// TestProbeRequestPatchInvariants probe 既有不变量不因 patch 改变：不跟随重定向仍是
// 结论来源（3xx 原样分类、只发一次），上游错误状态分类不变。
func TestProbeRequestPatchInvariants(t *testing.T) {
	spec := customProbeSpec(domain.FormatOpenAIChat)
	spec.Upstream = probePatchUpstream(t, mustProbePatch(t, "{\"headers\":{\"Host\":\"routing.example\",\"X-Patch\":\"1\"}}"))

	hits := 0
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		hits++
		return contentResponse(http.StatusFound, "{}"), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(), spec)
	require.False(t, res.Success)
	require.Equal(t, http.StatusFound, res.StatusCode, "3xx 原样作为失败结论（不跟随重定向）")
	require.Equal(t, domain.ProbeErrorUpstreamRedirect, res.Error)
	require.Equal(t, 1, hits, "不跟随重定向：只发一次请求")

	hits = 0
	rt = contentRT(func(req *http.Request) (*http.Response, error) {
		hits++
		return contentResponse(http.StatusUnauthorized, "{\"error\":{\"code\":\"invalid_api_key\"}}"), nil
	})
	res = newTestProber(t, rt).Probe(context.Background(), spec)
	require.Equal(t, 1, hits)
	require.Equal(t, domain.ProbeErrorUpstreamAuth, res.Error, "auth 分类不因 patch 改变")
	require.Equal(t, "code=invalid_api_key", res.Detail, "有限错误码 detail 不变")
}
