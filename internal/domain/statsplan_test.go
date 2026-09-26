// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// Walkthrough of the single judgement entry (`Admit`) against spec §8:
// A2 (zone-branch 三条判定), A3 (原因枚举精确 + 穷尽性), A7 (N1), A8 (N2),
// A9 (N3), A10 (N4), A10b (N5). 全部固定时钟（time.Date(2026,…)），零墙钟依赖。

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// statsPlanNow 固定判定时钟（coverage 之外的两步与 now 无关；此处零保留期故
// coverage 整体跳过——coverage 的时钟语义由 service 级用例钉死）。
var statsPlanNow = time.Date(2026, 9, 25, 15, 38, 21, 0, time.UTC)

// planFixtures 复用 statszone_test.go 的时区夹具。
func planFixtures(t *testing.T) (cst, ist, ny *time.Location) {
	t.Helper()
	var err error
	cst, err = time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	ist, err = time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	ny, err = time.LoadLocation("America/New_York")
	require.NoError(t, err)
	return cst, ist, ny
}

// TestAdmit_zoneBranch A2：三条判定各自的 Storage/Reason/窗口。
func TestAdmit_zoneBranch(t *testing.T) {
	cst, ist, ny := planFixtures(t)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	autumn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ret := Retention{} // 零保留期 = coverage 整体跳过（本用例只钉规划形状）

	cases := []struct {
		name       string
		zone       *time.Location
		from, to   time.Time
		wantStor   StatsStorage
		wantReason StatsPlanReason
		wantFrom   time.Time
		wantTo     time.Time
	}{
		{"整点整小时时区 → cube/Exact 且窗口不变", cst, aug, aug.Add(90 * 24 * time.Hour),
			StatsStorageCube, StatsPlanExact, aug, aug.Add(90 * 24 * time.Hour)},
		{"界不齐整小时时区（跨度 ≥1h）→ cube/WindowShifted",
			cst, aug.Add(30 * time.Minute), aug.Add(2*time.Hour + 30*time.Minute),
			StatsStorageCube, StatsPlanWindowShifted, aug.Add(time.Hour), aug.Add(3 * time.Hour)},
		{":30 偏移 → raw/Offset 且窗口等于请求窗口", ist, aug, aug.Add(24 * time.Hour),
			StatsStorageRaw, StatsPlanOffset, aug, aug.Add(24 * time.Hour)},
		{"跨 DST → raw/DST 且窗口等于请求窗口", ny, autumn, autumn.Add(3 * 24 * time.Hour),
			StatsStorageRaw, StatsPlanDST, autumn, autumn.Add(3 * 24 * time.Hour)},
		{"跨度 <1h → raw/SpanBelowGrid 且窗口等于请求窗口", cst, aug.Add(10 * time.Minute), aug.Add(20 * time.Minute),
			StatsStorageRaw, StatsPlanSpanBelowGrid, aug.Add(10 * time.Minute), aug.Add(20 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec, werr := Admit(KindTrend, ret, tc.zone, tc.from, tc.to, statsPlanNow)
			require.Nil(t, werr)
			require.Equal(t, tc.wantStor, exec.Storage)
			require.Equal(t, tc.wantReason, exec.Reason)
			require.True(t, exec.From.Equal(tc.wantFrom), "effective from %v", exec.From)
			require.True(t, exec.To.Equal(tc.wantTo), "effective to %v", exec.To)
		})
	}
}

// TestAdmit_rawReasonPreciseAndExhaustive A3：原因精确（不是布尔）+ 穷尽性——
// 第 3 条且跨度 ≥1h 的输入，Reason 恒 ∈ {Offset, DST}（原因取自 cand）。
func TestAdmit_rawReasonPreciseAndExhaustive(t *testing.T) {
	cst, ist, ny := planFixtures(t)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	autumn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ret := Retention{}

	fixed := []struct {
		name string
		zone *time.Location
		from time.Time
		to   time.Time
		want StatsPlanReason
	}{
		{"Kolkata 跨度 ≥1h → Offset", ist, aug, aug.Add(24 * time.Hour), StatsPlanOffset},
		{"NY 跨秋退 → DST", ny, autumn, autumn.Add(3 * 24 * time.Hour), StatsPlanDST},
		{"UTC 界偏 30 分 → 第 2 条命中（WindowShifted）", time.UTC, aug.Add(30 * time.Minute), aug.Add(30*time.Minute + 24*time.Hour), StatsPlanWindowShifted},
		{"Kolkata + 界不齐 → 仍为 Offset（不是 Unaligned；原因取自 cand）", ist, aug.Add(30 * time.Minute), aug.Add(30*time.Minute + 24*time.Hour), StatsPlanOffset},
		{"跨 DST + 界不齐 → DST（原因取自 cand）", ny, autumn.Add(30 * time.Minute), autumn.Add(30*time.Minute + 72*time.Hour), StatsPlanDST},
	}
	for _, tc := range fixed {
		t.Run(tc.name, func(t *testing.T) {
			exec, werr := Admit(KindTrend, ret, tc.zone, tc.from, tc.to, statsPlanNow)
			require.Nil(t, werr)
			require.Equal(t, tc.want, exec.Reason)
		})
	}

	// 穷尽性：zone × from 偏移 × 跨度 的笛卡尔积上，凡落第 3 条且跨度 ≥1h 者，
	// 原因必须落在 {Offset, DST}（实现若为 :30 偏移派生 Unaligned 即红）。
	zones := []*time.Location{cst, ist, ny, time.UTC}
	offsets := []time.Duration{0, time.Second, 30 * time.Minute, 59*time.Minute + 59*time.Second}
	spans := []time.Duration{time.Hour, 70 * time.Minute, 24 * time.Hour, 8 * 24 * time.Hour}
	for _, zone := range zones {
		for _, off := range offsets {
			for _, span := range spans {
				from := aug.Add(off)
				to := from.Add(span)
				exec, werr := Admit(KindTrend, ret, zone, from, to, statsPlanNow)
				require.Nil(t, werr)
				if exec.Storage != StatsStorageRaw {
					continue
				}
				require.Contains(t, []StatsPlanReason{StatsPlanOffset, StatsPlanDST}, exec.Reason,
					"zone=%v off=%v span=%v", zone, off, span)
			}
		}
	}
}

// planProbeCase 随机探针三元组（固定种子——零墙钟依赖、可复现）。
func planProbeCase(r *rand.Rand) (zone *time.Location, from, to time.Time) {
	cst, _ := time.LoadLocation("Asia/Shanghai")
	ist, _ := time.LoadLocation("Asia/Kolkata")
	ny, _ := time.LoadLocation("America/New_York")
	zones := []*time.Location{time.UTC, cst, ist, ny}
	zone = zones[r.Intn(len(zones))]
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// 1h ~ 8d 跨度（分组原始行的成本上限之内 → 任何时区的探针都被接受；更长
	// 跨度的 N3 由 TestAdmit_N3SpanWithinCap 的穷举段单独覆盖）；偏移 0 ~ 59m59s
	// （含 0 以覆盖"双界已齐"的 Exact 分支）。
	span := time.Duration(1+r.Intn(8*24)) * time.Hour
	off := time.Duration(r.Intn(3600))*time.Second + time.Duration(r.Intn(1000))*time.Millisecond
	if r.Intn(8) == 0 {
		off = 0 // 双界已齐（Exact 分支）也必须被随机段覆盖
	}
	from = base.Add(off)
	return zone, from, from.Add(span)
}

// TestAdmit_N1BackwardBounded A7：N1 —— 两端只向后且每端 < 1h（仅对齐分支会
// 移动；统一断言：from ≤ exec.From < from+1h、to ≤ exec.To < to+1h）。
func TestAdmit_N1BackwardBounded(t *testing.T) {
	r := rand.New(rand.NewSource(20260925))
	ret := Retention{}
	for i := 0; i < 200; i++ {
		zone, from, to := planProbeCase(r)
		exec, werr := Admit(KindTrend, ret, zone, from, to, statsPlanNow)
		require.Nil(t, werr, "case %d zone=%v", i, zone)
		require.False(t, exec.From.Before(from), "case %d: from 不得变早", i)
		require.False(t, exec.To.Before(to), "case %d: to 不得变早", i)
		require.Less(t, exec.From.Sub(from), time.Hour, "case %d: 每端 <1h", i)
		require.Less(t, exec.To.Sub(to), time.Hour, "case %d: 每端 <1h", i)
	}
}

// TestAdmit_N2Idempotent A8：N2 —— 幂等，两个分支都要断言（Raw 重入不得变 Exact）。
func TestAdmit_N2Idempotent(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	ret := Retention{}
	sawCube, sawShifted, sawRaw := false, false, false
	for i := 0; i < 200; i++ {
		zone, from, to := planProbeCase(r)
		exec, werr := Admit(KindTrend, ret, zone, from, to, statsPlanNow)
		require.Nil(t, werr, "case %d", i)
		again, werr := Admit(KindTrend, ret, zone, exec.From, exec.To, statsPlanNow)
		require.Nil(t, werr, "case %d 重入", i)
		require.Equal(t, exec.Storage, again.Storage, "case %d 重入存储必须相同", i)
		require.True(t, again.From.Equal(exec.From), "case %d 重入窗口必须相同", i)
		require.True(t, again.To.Equal(exec.To), "case %d 重入窗口必须相同", i)
		switch {
		case exec.Storage == StatsStorageCube && exec.Reason == StatsPlanExact:
			sawCube = true
			require.Equal(t, StatsPlanExact, again.Reason)
		case exec.Storage == StatsStorageCube:
			sawShifted = true
			require.Equal(t, StatsPlanExact, again.Reason, "对齐后的窗口已精确 → 重入 Exact")
		default:
			sawRaw = true
			require.Equal(t, StatsStorageRaw, again.Storage, "Raw 重入不得变 Exact")
		}
	}
	require.True(t, sawCube && sawShifted && sawRaw, "三分支（Exact/Shifted/Raw）都要被覆盖到")
}

// TestAdmit_N3SpanWithinCap A9：N3 —— 生效跨度不越该 storage 的成本上限（穷举
// 4×6 组合 + 随机补充；上限与生效跨度之差恒为网格的非负整数倍）。
func TestAdmit_N3SpanWithinCap(t *testing.T) {
	cst, _, _ := planFixtures(t)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cap := StatsKinds[KindTrend].CostCap
	spans := []time.Duration{
		MaxCubeSpan, MaxCubeSpan - time.Second, MaxCubeSpan - 30*time.Minute,
		MaxCubeSpan - time.Hour, time.Hour + 45*time.Minute, time.Second,
	}
	offsets := []time.Duration{0, time.Second, 30 * time.Minute, 59*time.Minute + 59*time.Second,
		59*time.Minute + 59*time.Second + 999*time.Millisecond}
	for _, span := range spans {
		for _, off := range offsets {
			from := aug.Add(off)
			exec, werr := Admit(KindTrend, Retention{}, cst, from, from.Add(span), statsPlanNow)
			require.Nil(t, werr, "span=%v off=%v", span, off)
			require.LessOrEqual(t, exec.To.Sub(exec.From), cap[exec.Storage],
				"span=%v off=%v storage=%v", span, off, exec.Storage)
		}
	}
	r := rand.New(rand.NewSource(99))
	for i := 0; i < 200; i++ {
		zone, from, to := planProbeCase(r)
		exec, werr := Admit(KindTrend, Retention{}, zone, from, to, statsPlanNow)
		require.Nil(t, werr, "case %d", i)
		require.LessOrEqual(t, exec.To.Sub(exec.From), cap[exec.Storage], "case %d", i)
	}
}

// TestAdmit_N4SpanPreservedWhenPossible A10：N4 三分支——
// ①网格整倍数跨度 ⇒ 严格相等；②非整倍数 + 需对齐 ⇒ 不相等（反例 10:30→12:15）；
// ③跨度 <1h ⇒ 平凡相等。附一条披露：非整倍数但双界已齐时不发生对齐 ⇒ 亦相等
// （此时没有可移动的边界）。
func TestAdmit_N4SpanPreservedWhenPossible(t *testing.T) {
	cst, _, _ := planFixtures(t)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	_, ist, _ := planFixtures(t)
	preserved := []struct {
		name string
		zone *time.Location
		from time.Time
		to   time.Time
	}{
		{"①网格整倍数（预设 24h）", cst, aug, aug.Add(24 * time.Hour)},
		{"①网格整倍数（90d 上界）", cst, aug, aug.Add(MaxCubeSpan)},
		{"①网格整倍数 + 界不齐（两端同步移动同一 δ）", cst, aug.Add(30 * time.Minute), aug.Add(2*time.Hour + 30*time.Minute)},
		{"①网格整倍数 + 降级 Raw（窗口原样）", ist, aug, aug.Add(24 * time.Hour)},
		{"③跨度 <1h（Raw 不改窗口）", cst, aug.Add(10 * time.Minute), aug.Add(20 * time.Minute)},
	}
	for _, tc := range preserved {
		t.Run(tc.name, func(t *testing.T) {
			exec, werr := Admit(KindTrend, Retention{}, tc.zone, tc.from, tc.to, statsPlanNow)
			require.Nil(t, werr)
			require.Equal(t, tc.to.Sub(tc.from), exec.To.Sub(exec.From))
		})
	}

	t.Run("②非整倍数 + 需对齐 ⇒ 跨度微变（实名反例）", func(t *testing.T) {
		from := time.Date(2026, 8, 1, 10, 30, 0, 0, time.UTC)
		to := time.Date(2026, 8, 1, 12, 15, 0, 0, time.UTC) // 105min
		exec, werr := Admit(KindTrend, Retention{}, cst, from, to, statsPlanNow)
		require.Nil(t, werr)
		require.Equal(t, StatsStorageCube, exec.Storage)
		require.Equal(t, StatsPlanWindowShifted, exec.Reason)
		require.NotEqual(t, to.Sub(from), exec.To.Sub(exec.From))
		require.Equal(t, 120*time.Minute, exec.To.Sub(exec.From)) // [11:00, 13:00)
	})
}

// TestAdmit_N5NeverEmpty A10b：N5 —— 永不为空读（含同小时窗口反例）。
func TestAdmit_N5NeverEmpty(t *testing.T) {
	cst, _, _ := planFixtures(t)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	t.Run("同小时窗口不塌成空窗", func(t *testing.T) {
		exec, werr := Admit(KindTrend, Retention{}, cst, aug.Add(10*time.Minute), aug.Add(20*time.Minute), statsPlanNow)
		require.Nil(t, werr)
		require.True(t, exec.To.After(exec.From), "计划必须非空")
		require.Equal(t, StatsStorageRaw, exec.Storage)
		require.Equal(t, StatsPlanSpanBelowGrid, exec.Reason)
	})

	t.Run("70 分钟窗口可命中 cube 且非空", func(t *testing.T) {
		exec, werr := Admit(KindTrend, Retention{}, cst, aug.Add(10*time.Minute), aug.Add(80*time.Minute), statsPlanNow)
		require.Nil(t, werr)
		require.True(t, exec.To.After(exec.From), "计划必须非空")
		require.Equal(t, StatsStorageCube, exec.Storage)
		require.Equal(t, StatsPlanWindowShifted, exec.Reason)
	})

	t.Run("亚秒精度窗口仍向后取整（N1 不得回落）", func(t *testing.T) {
		// 10:00:00.001 → 11:00（若把亚秒丢掉会误判"已整点"而回落 10:00 —— 比
		// 请求下界更早，违反 N1）。
		from := time.Date(2026, 8, 1, 10, 0, 0, 1_000_000, time.UTC)
		to := from.Add(2 * time.Hour)
		exec, werr := Admit(KindTrend, Retention{}, cst, from, to, statsPlanNow)
		require.Nil(t, werr)
		require.Equal(t, StatsStorageCube, exec.Storage)
		require.False(t, exec.From.Before(from), "下界不得变早（%v vs %v）", exec.From, from)
		require.False(t, exec.To.Before(to), "上界不得变早（%v vs %v）", exec.To, to)
		require.True(t, exec.From.Equal(time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)))
		require.True(t, exec.To.Equal(time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)))
	})

	t.Run("随机 200 组恒非空", func(t *testing.T) {
		r := rand.New(rand.NewSource(4242))
		for i := 0; i < 200; i++ {
			zone, from, to := planProbeCase(r)
			exec, werr := Admit(KindTrend, Retention{}, zone, from, to, statsPlanNow)
			require.Nil(t, werr, "case %d", i)
			require.True(t, exec.To.After(exec.From), "case %d", i)
		}
	})
}

// TestAdmit_overviewNeverWindowShifted A2b（spec §4.5 可证结论）：Overview 的两个
// 窗口计划**永远不会是 WindowShifted**——整点无 DST 时区下 day/from/to 都是本地
// 零点即 UTC 整点 ⇒ 第一条判定命中；非整点偏移（:30/:45）或跨 DST 时区下对齐后
// 的 cand 仍不精确 ⇒ 第三条判定。故 overview 无需窗口回显（P2）。表驱动：两个
// Exec（KindSummary [day,to) 与 KindDays [from,to)）的 Reason 只落在
// {Exact, Offset, DST}。
func TestAdmit_overviewNeverWindowShifted(t *testing.T) {
	cst, ist, ny := planFixtures(t)
	npt, err := time.LoadLocation("Asia/Kathmandu") // +5:45
	require.NoError(t, err)

	cases := []struct {
		name string
		zone *time.Location
		day  time.Time // 请求时区本地零点（= handler 的值）
		days int
	}{
		{"UTC 整点零点", time.UTC, time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), 30},
		{"Shanghai +8 整点零点", cst, time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC), 30},
		{"Kolkata +5:30 非整点零点", ist, time.Date(2026, 9, 24, 18, 30, 0, 0, time.UTC), 7},
		{"Kathmandu +5:45 非整点零点", npt, time.Date(2026, 9, 24, 18, 15, 0, 0, time.UTC), 7},
		{"NY 秋退日本地零点（25h 本地日）", ny, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), 7},
		{"NY 春进日本地零点（23h 本地日）", ny, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := tc.day.AddDate(0, 0, -(tc.days - 1))
			to := tc.day.AddDate(0, 0, 1)
			for _, w := range []struct {
				kind     StatsKindID
				from, to time.Time
			}{{KindSummary, tc.day, to}, {KindDays, from, to}} {
				exec, werr := Admit(w.kind, Retention{}, tc.zone, w.from, w.to, statsPlanNow)
				require.Nil(t, werr, "kind=%s", w.kind)
				require.NotEqual(t, StatsPlanWindowShifted, exec.Reason,
					"overview 永不位移（kind=%s zone=%v）", w.kind, tc.zone)
				require.Contains(t, []StatsPlanReason{StatsPlanExact, StatsPlanOffset, StatsPlanDST}, exec.Reason)
				if exec.Storage == StatsStorageRaw {
					require.True(t, exec.From.Equal(w.from) && exec.To.Equal(w.to), "raw 计划窗口即请求窗口")
				}
			}
		})
	}
}

// TestAdmit_windowInvalid 步骤 1：零值/倒序 → window_invalid（拒绝路径不发日志、
// 不产生 plan）。
func TestAdmit_windowInvalid(t *testing.T) {
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		from, to time.Time
	}{
		{"缺 from", time.Time{}, aug},
		{"缺 to", aug, time.Time{}},
		{"to == from", aug, aug},
		{"to < from", aug, aug.Add(-time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec, werr := Admit(KindTrend, Retention{}, time.UTC, tc.from, tc.to, statsPlanNow)
			require.NotNil(t, werr)
			require.Equal(t, StatsRejectWindowInvalid, werr.Reject)
			require.Equal(t, Exec{}, exec)
		})
	}
}

// TestStatsCapabilitiesProjectionGolden 金标准（spec §8 A16'④ 的数值面）：
// 固定证据部署保留期（log=2 / errlog=7 / stats=180，§4.4(a) 的示例正是取自该
// 部署）钉一份**逐字段字面量**的投影期望——任何静默的矩阵数值改动都会红。
//
// 这是 7776000/691200/604800 这三个数字的**唯一**落点：能力端点（internal/handler）
// 内不得出现它们（A16'② 的零命中审计主张端点不写字面量），故端点侧的用例只钉
// 形状与"随保留期动/不随保留期动"，数值金标准落在矩阵的家乡。
func TestStatsCapabilitiesProjectionGolden(t *testing.T) {
	got := StatsCapabilities(Retention{Log: 2, ErrLog: 7, Stats: 180})
	require.Equal(t, int64(3600), got.BucketGridSeconds, "网格 = 1h")

	zoned := KindCapability{
		Grouping:       GroupingZoned,
		Storages:       []StatsStorage{StatsStorageCube, StatsStorageRaw},
		CostCapSeconds: map[StatsStorage]int64{StatsStorageCube: 7776000, StatsStorageRaw: 691200},
		CoverageDays:   map[StatsStorage]int{StatsStorageCube: 180, StatsStorageRaw: 2},
	}
	require.Equal(t, zoned, got.Kinds[KindTrend])
	require.Equal(t, zoned, got.Kinds[KindEntityTrend])
	require.Equal(t, zoned, got.Kinds[KindSummary])
	require.Equal(t, zoned, got.Kinds[KindDays])

	cubeOnly := KindCapability{
		Grouping:       GroupingNone,
		Storages:       []StatsStorage{StatsStorageCube},
		CostCapSeconds: map[StatsStorage]int64{StatsStorageCube: 7776000},
		CoverageDays:   map[StatsStorage]int{StatsStorageCube: 180},
	}
	require.Equal(t, cubeOnly, got.Kinds[KindTop])
	require.Equal(t, cubeOnly, got.Kinds[KindTTFTSketch])

	require.Equal(t, KindCapability{
		Grouping:       GroupingNone,
		Storages:       []StatsStorage{StatsStorageRaw},
		CostCapSeconds: map[StatsStorage]int64{StatsStorageRaw: 604800},
		CoverageDays:   map[StatsStorage]int{StatsStorageRaw: 2},
	}, got.Kinds[KindTTFTExact])
	// usage_agg 复用 cube 的 90d 常量（不新造常量），coverage 取 usage_logs 的单表保留期。
	require.Equal(t, KindCapability{
		Grouping:       GroupingNone,
		Storages:       []StatsStorage{StatsStorageRaw},
		CostCapSeconds: map[StatsStorage]int64{StatsStorageRaw: 7776000},
		CoverageDays:   map[StatsStorage]int{StatsStorageRaw: 2},
	}, got.Kinds[KindUsageAgg])
	for _, id := range []StatsKindID{KindUsageList, KindErrlogList} {
		require.Zero(t, got.Kinds[id].CostCapSeconds[StatsStorageRaw], "%s 无上限（keyset 分页）", id)
	}
	require.Equal(t, 2, got.Kinds[KindUsageList].CoverageDays[StatsStorageRaw], "只读 usage_logs")
	require.Equal(t, 7, got.Kinds[KindErrlogList].CoverageDays[StatsStorageRaw], "只读 err_logs")

	// 矩阵的**每个** kind 都必须被上面逐条认领（新增 kind 而不认领 → 本断言红）。
	require.Len(t, got.Kinds, len(StatsKinds))
	claimed := []StatsKindID{KindTrend, KindEntityTrend, KindSummary, KindDays, KindTop,
		KindTTFTSketch, KindTTFTExact, KindUsageAgg, KindUsageList, KindErrlogList}
	require.Len(t, claimed, len(StatsKinds), "金标准必须覆盖矩阵全部 kind")
	for _, id := range claimed {
		require.Contains(t, got.Kinds, id)
	}

	// cost 与 coverage 解耦：保留期禁用只让 coverage 归零，cost 恒为常量。
	off := StatsCapabilities(Retention{})
	require.Equal(t, zoned.CostCapSeconds, off.Kinds[KindTrend].CostCapSeconds, "保留期禁用不改 cost")
	for _, s := range zoned.Storages {
		require.Equal(t, zoned.CostCapSeconds[s], off.Kinds[KindTrend].CostCapSeconds[s])
		require.Zero(t, off.Kinds[KindTrend].CoverageDays[s], "守卫关闭 ⇒ 0（不是「只能查 0 天」）")
	}
	// 保留期只影响 coverage（log=30 使双表 min 从 2 变 7——errlog 仍为 7）。
	long := StatsCapabilities(Retention{Log: 30, ErrLog: 7, Stats: 180})
	require.Equal(t, long.Kinds[KindTrend].CostCapSeconds, zoned.CostCapSeconds, "cost 不随保留期变")
	require.Equal(t, 7, long.Kinds[KindTrend].CoverageDays[StatsStorageRaw])
	require.Equal(t, 180, long.Kinds[KindTrend].CoverageDays[StatsStorageCube], "cube 只读 usage_stats")
}

// TestStatsCapabilitiesIsBoundToMatrix 投影与判定读同一份内存（构造同源）：
// 对矩阵里每个 kind 的每个候选存储，投影出的上限秒数必须等于 Admit 实际用来
// 判定的 CostCap（改任一侧而不同步改另一侧在构造上不可能——本用例把它写成断言）。
func TestStatsCapabilitiesIsBoundToMatrix(t *testing.T) {
	caps := StatsCapabilities(Retention{})
	for id, k := range StatsKinds {
		for _, s := range k.Storages {
			require.Equal(t, int64(k.CostCap[s]/time.Second), caps.Kinds[id].CostCapSeconds[s], "%s/%s", id, s)
		}
	}
	// 上限的可见语义：0 = 无上限（logs keyset 分页），非 0 = 该存储的最大跨度。
	require.Zero(t, caps.Kinds[KindUsageList].CostCapSeconds[StatsStorageRaw])
	require.NotZero(t, caps.Kinds[KindTrend].CostCapSeconds[StatsStorageRaw])
}
