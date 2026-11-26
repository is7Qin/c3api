// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

// request_patch probe 面热路径成本（spec §7.1/§7.3/§10 P5）：
// 无配置 0 分配是硬断言（TestProbeRequestPatchHelpersAllocs）；本文件只记录数值，
// 不进硬门禁——与 proxy 的 request_patch_bench_test.go 同口径。
//
// 关键结论：无配置时 helper 不构造闭包/不解析/不复制（0 allocs）；有配置时每 probe
// 至多 1 枚闭包（构造期一次性），body splice 只在「本格式有 body 配置」时发生。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
)

// benchProbePatchSpec 有配置的探测 spec（头 + body 两层）。
func benchProbePatchSpec() domain.ProbeSpec {
	spec := customProbeSpec(domain.FormatOpenAIChat)
	spec.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Headers: map[string]string{"User-Agent": "vibe-code/1.0", "X-Call-Source": "vibe_code"},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathChat: {
				Headers: map[string]string{"OpenAI-Beta": "responses=v2"},
				Set:     map[string]json.RawMessage{"metadata.call_source": json.RawMessage("\"vibe_code\"")},
				Delete:  []string{"metadata.client_field"},
			},
		},
	}}
	return spec
}

// BenchmarkProbeRequestPatchNoConfig 无配置：装饰器 nil + body 原 slice，0 分配。
func BenchmarkProbeRequestPatchNoConfig(b *testing.B) {
	spec := domain.ProbeSpec{}
	body := []byte("{\"model\":\"gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if probeHeaderDecorator(spec, domain.UpstreamPathChat) != nil {
			b.Fatal("无配置必须 nil")
		}
		if got := (&Prober{}).applyBodyPatch(spec, domain.UpstreamPathChat, body); len(got) != len(body) {
			b.Fatal("无配置必须原样返回")
		}
	}
}

// BenchmarkProbeRequestPatchConfiguredHeader 有头配置：每 probe 1 枚闭包（sink 持有
// ⇒ 与生产一致地逃逸到堆）。
func BenchmarkProbeRequestPatchConfiguredHeader(b *testing.B) {
	spec := benchProbePatchSpec()
	var sink func(*http.Request)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = probeHeaderDecorator(spec, domain.UpstreamPathChat)
	}
	if sink == nil {
		b.Fatal("有配置必须返回装饰器")
	}
}

// BenchmarkProbeRequestPatchConfiguredBody 有 body 配置：delete 排序 → set 排序的
// sjson splice（执行器唯一成本）。
func BenchmarkProbeRequestPatchConfiguredBody(b *testing.B) {
	spec := benchProbePatchSpec()
	body := []byte("{\"model\":\"gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"metadata\":{\"client_field\":\"x\"}}")
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if got := (&Prober{}).applyBodyPatch(spec, domain.UpstreamPathChat, body); len(got) == 0 {
			b.Fatal("patch 后 body 为空")
		}
	}
}
