// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// routeBenchProductionAccounts 是 U22（第二步）的**生产形状**夹具：accounts 个
// 账号摊在 templates 枚模板上（i % templates），同一模板的所有账号共享**同一枚**
// *domain.Template 指针——第一步（35142e74）之后真实载入器的形状。
//
// 与 U8 的 routeBenchDomainAccounts（每账号一枚独立模板、顺序恶意）**刻意分离**：
// 那个夹具是"指针去重作弊"的反向仪器，U22 的验收只用本夹具（spec §4.2）。
// 每模板内容与 U8 的 shadow 同形（99 个模型 + anthropic 白名单），使每叶的模板
// 摘要成本 ≈209 allocs —— before 臂因此可与其 672,000 基线直接对照。
func routeBenchProductionAccounts(accounts, templates int) []*domain.Account {
	if accounts <= 0 || templates <= 0 {
		return nil
	}
	models := make([]string, 0, 99)
	for i := 0; i < 99; i++ {
		models = append(models, fmt.Sprintf("m-%03d", i))
	}
	formats := domain.RoutableFormats()
	tpls := make([]*domain.Template, templates)
	for i := range tpls {
		tpls[i] = &domain.Template{
			SupportedFormats: slices.Clone(formats),
			Models:           slices.Clone(models),
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: slices.Clone(models)},
		}
	}
	accs := make([]*domain.Account, 0, accounts)
	for i := range accounts {
		accs = append(accs, &domain.Account{
			ID: int64(i + 1), Template: tpls[i%templates], UpstreamKey: "k",
			Enabled: true, MaxConcurrency: 2, LifecycleRevision: 1, IdentityRevision: 1,
		})
	}
	return accs
}

// BenchmarkGroupRebuildProdT100A3000 / T89A3000 是 U22 的 before/after：
//
//	after  = buildSnapshots(..., 一张 reload 作用域的摘要表) + attachCompilerFacts
//	before = 同一条路径但传 nil（= 改动前的逐叶重算，逐字节同样的派生代码）
//
// 两条臂共用同一份夹具与同一段代码，唯一变量是缓存参数 ⇒ 差值就是本单元的全部
// 收益。T/A/A÷T 写进基准名与日志：**"allocs ≤ 1/10"只在每模板 ≥34 账号时成立**。
func BenchmarkGroupRebuildProdT100A3000(b *testing.B) {
	benchmarkGroupRebuildProd(b, 3000, 100)
}

func BenchmarkGroupRebuildProdT89A3000(b *testing.B) {
	benchmarkGroupRebuildProd(b, 3000, 89)
}

func benchmarkGroupRebuildProd(b *testing.B, accounts, templates int) {
	b.Helper()
	byGroup := map[int64][]*domain.Account{10: routeBenchProductionAccounts(accounts, templates)}
	b.Logf("生产形状：T=%d A=%d A÷T=%.2f", templates, accounts, float64(accounts)/float64(templates))
	b.Run("after/per-reload-digest-cache", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			groups, byID, err := buildSnapshots(byGroup, nil, testEgressPool, newTemplateDigestCache())
			require.NoError(b, err)
			routeBenchSink.groups = groups
			routeBenchSink.facts = attachCompilerFacts(byID)
		}
	})
	b.Run("before/per-leaf-recompute", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			groups, byID, err := buildSnapshots(byGroup, nil, testEgressPool, nil)
			require.NoError(b, err)
			routeBenchSink.groups = groups
			routeBenchSink.facts = attachCompilerFacts(byID)
		}
	})
}
