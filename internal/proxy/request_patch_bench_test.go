// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// request_patch 热路径成本对照（spec §7.1/§9 P3：bench 记录，不进硬门禁；
// 无配置 0 新增分配是 A5/§7.1 的验收项，由 TestRequestPatchHelpersNoConfigZeroAllocs
// 硬断言 + 本文件的 bench 数值双证）。

import (
	"encoding/json"
	"net/http"
	"testing"

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
