// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/supplier"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// workerSlotIndex 取注册序里某个槽的下标（-1 = 不存在）。
func workerSlotIndex(name string) int {
	for i, s := range workerOrder {
		if s.name == name {
			return i
		}
	}
	return -1
}

// TestWorkerOrderSingleOwner 注册序只允许有一个主语：表定义在 wiring_run.go，
// 其它生产文件不得另抄一份（两份表必然漂移，而顺序错了没有任何运行期门能看见）。
func TestWorkerOrderSingleOwner(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		n := strings.Count(readSrcFile(t, e.Name()), "workerOrder")
		if e.Name() == "wiring_run.go" {
			require.GreaterOrEqual(t, n, 1, "wiring_run.go 必须定义 workerOrder")
			continue
		}
		require.Zero(t, n, "%s 不得引用 workerOrder（注册序只允许一个主语）", e.Name())
	}
}

// frozenWorkerOrder 是注册序的冻结基线（来源 `354b6082`：wiring_run.go:145/148/149
// 的 orderedWorkers + 两次 append，以及 6 处 wm.Register 的先后）。
//
// 改这张表 = 改生命周期语义：注册反序即排空反序，停机时先关谁后关谁全靠它。
// 所以它必须是**字面量**，不得由生产符号生成（否则改代码即改契约）。
var frozenWorkerOrder = []string{
	"mailW", "warningWorker", "billingWorker", "inv", "schedW", "ruleEngine",
	"retryWorker", "healthW", "supplierWorkers", "rec", "errlogW", "contBindW",
	"convW", "quotaW", "pricingSync", "retention", "statsAgg", "qualityFlowOwner",
	"qualitySync", "supplierViewLoader", "concSync", "accConcSync", "disco",
	"listener", "authSync",
}

// TestWorkerOrderTable 钉住表本身：名字序列、可缺席槽恰好 4 个、可展开槽只有一个。
func TestWorkerOrderTable(t *testing.T) {
	got := make([]string, 0, len(workerOrder))
	skippable := 0
	expanded := 0
	for _, s := range workerOrder {
		got = append(got, s.name)
		if s.skipWhen != nil {
			skippable++
		}
		if s.expand {
			expanded++
			require.Equal(t, "supplierWorkers", s.name, "只有 supplier 组允许展开")
		}
	}
	require.Equal(t, frozenWorkerOrder, got, "注册序与冻结基线不一致：改顺序即改排空语义")
	require.Equal(t, 4, skippable, "可缺席槽必须恰 4 个（warning/billing/supplierWorkers/supplierViewLoader）")
	require.Equal(t, 1, expanded, "可展开槽必须恰 1 个")
}

// fullWorkerSet 造一套「全部就绪」的 workerSet（含 supplier 组的两个 worker）。
func fullWorkerSet() workerSet {
	set := workerSet{byName: map[string]worker.Worker{}, groups: map[string][]worker.Worker{}}
	for _, s := range workerOrder {
		if s.expand {
			continue
		}
		set.byName[s.name] = namedWorker{name: s.name}
	}
	set.groups["supplierWorkers"] = []worker.Worker{
		namedWorker{name: "supplier-thaw"},
		namedWorker{name: "supplier-credit"},
	}
	return set
}

func enabledCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Billing.Enabled = true
	cfg.Supplier.Enabled = true
	return cfg
}

// TestAssembleWorkersOrder 是「唯一行为证据」的地基：装配输出必须与注册序逐项相等。
//
// 它封住三类只靠表断言抓不到的写法：反向遍历、`plan[:len-2]` 截断、循环里
// `if w == nil { continue }`（后者会把装配缺陷吞成静默跳过）。
func TestAssembleWorkersOrder(t *testing.T) {
	plan := assembleWorkers(fullWorkerSet(), enabledCfg())

	got := make([]string, 0, len(plan))
	for _, e := range plan {
		got = append(got, e.name)
		require.NotNil(t, e.w, "plan 里出现 nil worker")
	}
	// supplier 组在 plan 里展开成两个条目（都沿用组名），而 frozenWorkerOrder 是
	// **槽**序列（一个占位），所以期望序列要显式展开，不能直接比。
	expected := make([]string, 0, len(frozenWorkerOrder)+1)
	for _, n := range frozenWorkerOrder {
		expected = append(expected, n)
		if n == "supplierWorkers" {
			expected = append(expected, n)
		}
	}
	require.Equal(t, expected, got)
	require.Len(t, plan, len(expected))
}

// TestAssembleWorkersAbsence 钉住「缺席」的三态 + cfg 与装配不一致。
func TestAssembleWorkersAbsence(t *testing.T) {
	t.Run("键在但值为nil且cfg允许缺席则跳过", func(t *testing.T) {
		cfg := enabledCfg()
		cfg.Billing.Enabled = false
		set := fullWorkerSet()
		set.byName["warningWorker"] = nil // 键在、值为 nil：关闭态的真实形状
		set.byName["billingWorker"] = nil
		plan := assembleWorkers(set, cfg)
		require.NotContains(t, entryNames(plan), "warningWorker")
		require.NotContains(t, entryNames(plan), "billingWorker")
	})

	t.Run("键在但值为nil且不可缺席则panic", func(t *testing.T) {
		set := fullWorkerSet()
		set.byName["rec"] = nil
		require.Panics(t, func() { assembleWorkers(set, enabledCfg()) })
	})

	t.Run("键缺失且不可缺席则panic", func(t *testing.T) {
		set := fullWorkerSet()
		delete(set.byName, "authSync")
		require.Panics(t, func() { assembleWorkers(set, enabledCfg()) })
	})

	t.Run("cfg说该关却给了值则panic", func(t *testing.T) {
		cfg := enabledCfg()
		cfg.Billing.Enabled = false
		require.Panics(t, func() { assembleWorkers(fullWorkerSet(), cfg) }, "billing 关闭却仍有 warningWorker/billingWorker")
	})

	t.Run("供应商关闭时组缺席合法", func(t *testing.T) {
		cfg := enabledCfg()
		cfg.Supplier.Enabled = false
		set := fullWorkerSet()
		delete(set.groups, "supplierWorkers")
		delete(set.byName, "supplierViewLoader")
		plan := assembleWorkers(set, cfg)
		require.NotContains(t, entryNames(plan), "supplierWorkers")
	})

	t.Run("供应商开启但组为空则panic", func(t *testing.T) {
		set := fullWorkerSet()
		delete(set.groups, "supplierWorkers")
		require.Panics(t, func() { assembleWorkers(set, enabledCfg()) }, "enabled=true 但 credit 未装配必须炸")
	})
}

// TestOptionalWorkerSlotsUseAbsenceNotTypedNil 钉住可选槽只允许一种写法。
//
// 生产装配（buildRuntime）对可选 worker 一律「不塞键」表达缺席；绝不能写成
// 「nil 也塞进 map」：`var p *supplier.ViewLoader = nil` 装进 map[string]worker.Worker
// 是**非 nil 接口**（动态类型就绪、值才是 nil），assembleWorkers 的缺席判据
// `!ok || w == nil` 读不到它，于是「关闭态」被判成「cfg 说该关却给了值」并 panic
// ——默认配置（supplier.enabled=false，也是 deploy/config.toml 的取值）曾在启动
// 时就这么崩。
func TestOptionalWorkerSlotsUseAbsenceNotTypedNil(t *testing.T) {
	var loader *supplier.ViewLoader // 关闭态的真实形状：typed nil

	cfg := enabledCfg()
	cfg.Supplier.Enabled = false
	set := fullWorkerSet()
	delete(set.groups, "supplierWorkers")

	// 生产写法：关闭态 ⇒ 键根本不出现 ⇒ 合法缺席，不 panic。
	delete(set.byName, "supplierViewLoader")
	require.NotPanics(t, func() { assembleWorkers(set, cfg) })

	// 变异：把 typed nil 也塞进 map ⇒ fail-fast 必须红（缺席 vs 有值仍被区分）。
	set.byName["supplierViewLoader"] = loader
	require.Panics(t, func() { assembleWorkers(set, cfg) })
}

// TestOptionalWorkerSlotsAreNilGuarded 是上一条的结构面：装配点必须用
// 「具体指针判 nil 再 byName[...] = ...」，不能把可选槽写回 map 字面量。
//
// 为什么用文本门而不是行为门：buildRuntime 需要整棵装配图（PG/Redis），单元测试
// 到不了那里；而这条规则的失效形态（typed nil 进 map）在启动期才炸，且默认配置
// 就是失效态。文本门读的是本包唯一的装配点，改法只有两种：保持 if 守卫，或者
// 明知故犯地把键写回字面量——后者在 diff 里显式可见。
func TestOptionalWorkerSlotsAreNilGuarded(t *testing.T) {
	src := readSrcFile(t, "wiring_run.go")
	for _, slot := range []string{"warningWorker", "billingWorker", "supplierViewLoader"} {
		require.Contains(t, src, "if "+slot+" != nil {",
			"可选槽 %s 必须先判 nil：typed nil 装进 worker.Worker 是非 nil 接口，assembleWorkers 会 panic", slot)
		require.Contains(t, src, `byName["`+slot+`"] = `+slot,
			"可选槽 %s 只能在 if 守卫内写入 byName（缺席 = 不塞键）", slot)
	}
}

// statsWorker 同时满足 worker.Worker 与 handler.StatsProvider。
type statsWorker struct {
	namedWorker
	stats any
}

func (s statsWorker) Stats() any { return s.stats }

// TestBuildOps 钉住 ops 候选集 = plan 派生 + extra，且 dispatch-observer 恰一项。
//
// 为什么单独测：dispatchObserver 只进 ops、不进注册，plan 化时最容易被「ops = plan」
// 顺手吃掉；它是「观测拒收」的唯一可查痕迹，而现有测试对它零覆盖。
func TestBuildOps(t *testing.T) {
	plan := []workerEntry{
		{name: "a", w: statsWorker{namedWorker: namedWorker{name: "a"}, stats: 1}},
		{name: "b", w: statsWorker{namedWorker: namedWorker{name: "b"}, stats: 2}},
	}
	extra := []handler.StatsProvider{statsWorker{namedWorker: namedWorker{name: "dispatch-observer"}, stats: 3}}

	got := buildOps(plan, extra, (*logx.Logger)(nil))
	require.Len(t, got, len(plan)+len(extra))

	names := make([]string, 0, len(got))
	observer := 0
	for _, p := range got {
		names = append(names, p.Name())
		if p.Name() == "dispatch-observer" {
			observer++
		}
	}
	require.Equal(t, []string{"a", "b", "dispatch-observer"}, names)
	require.Equal(t, 1, observer, "dispatch-observer 必须恰一项")
}

// TestSupplierWorkersCreditRequired 钉住资金面的 fail-fast：enabled 时 credit 不可缺。
func TestSupplierWorkersCreditRequired(t *testing.T) {
	thaw := namedWorker{name: "supplier-thaw"}
	require.Panics(t, func() { supplierWorkersFor(true, false, nil, nil) })
	require.Panics(t, func() { supplierWorkersFor(true, true, thaw, nil) })
	require.NotPanics(t, func() { supplierWorkersFor(true, true, nil, namedWorker{name: "credit"}) })
}

func entryNames(plan []workerEntry) []string {
	out := make([]string, 0, len(plan))
	for _, e := range plan {
		out = append(out, e.name)
	}
	return out
}

var _ worker.Worker = statsWorker{}
var _ handler.StatsProvider = statsWorker{}

// 编译期：assembleWorkers 的签名必须接受 config（skipWhen 谓词的输入）。
var _ = func(cfg *config.Config) []workerEntry { return assembleWorkers(workerSet{}, cfg) }
