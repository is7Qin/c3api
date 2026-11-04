// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

// benchBody 构造典型非流 chat 请求体（pad 填充 messages 内容以模拟大请求）。
func benchBody(pad int) []byte {
	return []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"` +
		strings.Repeat("x", pad) + `"}],"temperature":0.2}`)
}

// BenchmarkDuplicateTopLevelKey 入口重复键扫描的每请求成本（新增开销：
// 一次全文档单遍扫描；无敏感键重复时零分配）。
func BenchmarkDuplicateTopLevelKey(b *testing.B) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"1KB", benchBody(900)},
		{"64KB", benchBody(64 << 10)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.body)))
			for i := 0; i < b.N; i++ {
				if k := duplicateTopLevelKey(tc.body, sensitiveBodyKeys); k != "" {
					b.Fatalf("unexpected duplicate %q", k)
				}
			}
		})
	}
}

// BenchmarkPathOf 路径覆盖查找的成本（custom 模板命中一次 map 读；未配置回落
// 常量）。热路径每次上游调用一次。
func BenchmarkPathOf(b *testing.B) {
	custom := &scheduler.Selection{
		CredentialType: credential.TypeCustom,
		UpstreamPaths:  map[string]string{pathKeyChat: "v1/chat/completions"},
	}
	plain := &scheduler.Selection{CredentialType: credential.TypeAPIKey}
	b.Run("custom-hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := pathOf(custom, pathKeyChat, defaultPathChat); got != "v1/chat/completions" {
				b.Fatalf("got %q", got)
			}
		}
	})
	b.Run("unconfigured", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := pathOf(plain, pathKeyChat, defaultPathChat); got != defaultPathChat {
				b.Fatalf("got %q", got)
			}
		}
	})
}
