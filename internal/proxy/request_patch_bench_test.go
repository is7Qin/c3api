// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// request_patch 热路径成本对照（spec §7.1/§7.2/§9 P3+P4：bench 记录，不进硬门禁；
// 无配置 0 新增分配是 A5/§7.1/§7.2 的验收项，由
// TestRequestPatchHelpersNoConfigZeroAllocs（HTTP）与
// TestRequestPatchWSHelpersNoConfigZeroAllocs（WS）硬断言 + 本文件的 bench 数值双证）。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/coder/websocket"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// benchPatchSelection 构造无配置/有配置两种 Selection（热路径两个档位）。
func benchPatchSelection(configured bool) *api.Selection {
	sel := &api.Selection{CredentialType: credential.TypeCustom, UpstreamKey: "sk-bench"}
	if !configured {
		return sel
	}
	sel.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Headers: map[string]string{"User-Agent": "vibe-code/1.0", "X-Call-Source": "vibe_code", "X-Drop": ""},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathChat: {
				Headers: map[string]string{"OpenAI-Beta": "responses=v2"},
				Set: map[string]json.RawMessage{
					"metadata.call_source": json.RawMessage(`"vibe_code"`),
					"max_output_tokens":    json.RawMessage(`4096`),
				},
				Delete: []string{"metadata.client_field", "temperature"},
			},
		},
	}}
	return sel
}

// BenchmarkRequestPatchNoConfig 无配置：headerDecorator 返回 nil + bodyPatch 原切片，
// 每 attempt 0 分配（bench 数值应与「未引入本特性」逐字节同档）。
func BenchmarkRequestPatchNoConfig(b *testing.B) {
	sel := benchPatchSelection(false)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	p := &Proxy{}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if headerDecorator(sel, patchKeyChat, "sk-bench") != nil {
			b.Fatal("无配置必须 nil")
		}
		if got := p.bodyPatch(sel, patchKeyChat, body); len(got) != len(body) {
			b.Fatal("无配置必须原样返回")
		}
	}
}

// BenchmarkRequestPatchConfiguredHeader 有配置头：一次闭包构造 + 逐头应用
// （Header.Set 的分配属 net/http/textproto 既有语义，单列观察）。
func BenchmarkRequestPatchConfiguredHeader(b *testing.B) {
	sel := benchPatchSelection(true)
	req := &http.Request{Header: make(http.Header, 8)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dec := headerDecorator(sel, patchKeyChat, "sk-bench")
		dec(req)
	}
}

// BenchmarkRequestPatchConfiguredBody 有配置 body：delete 排序 → set 排序的
// sjson splice（执行器唯一成本；限频 WARN 状态只在失败路径）。
func BenchmarkRequestPatchConfiguredBody(b *testing.B) {
	sel := benchPatchSelection(true)
	p := &Proxy{}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"metadata":{"client_field":"x"},"temperature":0.2}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if got := p.bodyPatch(sel, patchKeyChat, body); len(got) == 0 {
			b.Fatal("patch 后 body 为空")
		}
	}
}

// benchWSPatchSelection 构造 WS 面两档 Selection：无配置 / 有头+body 配置。
func benchWSPatchSelection(configured bool) *api.Selection {
	sel := &api.Selection{CredentialType: credential.TypeCustom, UpstreamKey: "sk-bench"}
	if !configured {
		return sel
	}
	sel.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Headers: map[string]string{"User-Agent": "vibe-code/1.0", "X-Call-Source": "vibe_code"},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathResponsesWS: {
				Headers: map[string]string{"OpenAI-Beta": "responses=v2"},
				Set:     map[string]json.RawMessage{"metadata.call_source": json.RawMessage(`"vibe_code"`)},
				Delete:  []string{"metadata.client_field"},
			},
		},
	}}
	return sel
}

// BenchmarkRequestPatchWSNoConfig 无配置 WS 面：wsHeaderDecorator / wsClientFrameHook
// 均 nil、每 attempt 0 分配（应与会话未引入本特性时逐字节同档）。
func BenchmarkRequestPatchWSNoConfig(b *testing.B) {
	sel := benchWSPatchSelection(false)
	p := &Proxy{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if wsHeaderDecorator(sel, patchKeyResponsesWS, "sk-bench") != nil {
			b.Fatal("无配置必须 nil")
		}
		if p.wsClientFrameHook(sel) != nil {
			b.Fatal("无配置必须 nil")
		}
	}
}

// BenchmarkRequestPatchWSNonCreateFrames 有配置但帧不是 response.create：首键扫描后
// 直接返回 nil（零分配、不复制、不解析 JSON）。
func BenchmarkRequestPatchWSNonCreateFrames(b *testing.B) {
	sel := benchWSPatchSelection(true)
	p := &Proxy{}
	hook := p.wsClientFrameHook(sel)
	if hook == nil {
		b.Fatal("有配置必须返回 hook")
	}
	frames := [][]byte{
		[]byte(`{"type":"response.cancel","response_id":"rsp_1"}`),
		[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		[]byte("not-json"),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := hook(frames[i%len(frames)]); got != nil {
			b.Fatal("非 create 帧必须原样转发")
		}
	}
}

// BenchmarkRequestPatchWSCreateFrame 有配置 + response.create：唯一执行 body patch
// 的路径（delete 排序 → set 排序的 sjson splice 成本）。
func BenchmarkRequestPatchWSCreateFrame(b *testing.B) {
	sel := benchWSPatchSelection(true)
	p := &Proxy{}
	hook := p.wsClientFrameHook(sel)
	if hook == nil {
		b.Fatal("有配置必须返回 hook")
	}
	frame := []byte(`{"type":"response.create","model":"gpt-4o","input":"hi","metadata":{"client_field":"x"}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for i := 0; i < b.N; i++ {
		if got := hook(frame); len(got) == 0 {
			b.Fatal("create 帧 patch 后不得为空")
		}
	}
}

// BenchmarkRequestPatchWSHandshakeConfigured 有配置握手：一次闭包构造 + 应用到
// DialOptions（Header.Set 的分配属 textproto 既有语义，单列观察）。
func BenchmarkRequestPatchWSHandshakeConfigured(b *testing.B) {
	sel := benchWSPatchSelection(true)
	opts := &websocket.DialOptions{HTTPHeader: make(http.Header, 4)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dec := wsHeaderDecorator(sel, patchKeyResponsesWS, "sk-bench")
		if dec == nil {
			b.Fatal("有配置必须返回装饰器")
		}
		dec(opts)
	}
}
