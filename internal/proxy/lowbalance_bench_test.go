// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// BenchmarkGuardPipeline 低余额钳制开关对 guardPipeline 热路径的分配影响：
// cap_off（baseline，cap=0）与 cap_on（treat，cap=5），同夹具同输入。
// 每轮成功即 p.auth.Release 复位门禁槽——防有限上限下撞 429 分支使 allocs 失真。
// 断言见 TestGuardPipelineLowBalanceAllocsParity（delta==0）。
func BenchmarkGuardPipeline(b *testing.B) {
	for _, c := range []struct {
		name    string
		capConc int
	}{
		{"cap_off", 0},
		{"cap_on", 5},
	} {
		b.Run(c.name, func(b *testing.B) {
			// 余额 $5（<$10 触发钳制）；原上限 10（> cap，确保 cap_on 走钳制分支）。
			p := lowBalGateProxy(b, c.capConc, lowBalThresholdMilli, 500_000, 10)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, rm, level, ok := p.guardPipeline(w, req, domain.FormatOpenAIChat, "r", time.Now(), true)
				if !ok {
					b.Fatalf("guardPipeline 拒绝（夹具须保持可准入）")
				}
				p.auth.Release(rm.meta, level) // 复位门禁槽
			}
		})
	}
}

// BenchmarkLowBalanceEffectiveCap 纯函数 capLowBalanceUserConc（钳制判定核心）
// 零分配基准——AllocsPerRun==0 由 TestCapLowBalanceUserConcZeroAlloc 钉死。
func BenchmarkLowBalanceEffectiveCap(b *testing.B) {
	b.ReportAllocs()
	var sink int
	for i := 0; i < b.N; i++ {
		sink += capLowBalanceUserConc(10, 5)
	}
	_ = sink
}

// BenchmarkConcurrencyGateAcquire 门禁 acquire/release 回归护栏（未改动，证据弱）：
// 记录数值以备与历史对比（每轮复位槽位，稳态不撞 429）。
func BenchmarkConcurrencyGateAcquire(b *testing.B) {
	g := newConcurrencyGate(nil, true)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 10, KeyMaxConc: 10}
	g.upsert(meta)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		level, ok := g.acquire(meta)
		if !ok {
			b.Fatal("acquire 拒绝（夹具须保持可准入）")
		}
		g.release(meta, level)
	}
}
