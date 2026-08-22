// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// TTFT 双分支真实 PG 测试（spec 2026-08-23 §7.7）：exact 分支固定种子已知分布
// → percentile_cont 精确断言；sketch 分支与 hist 插值数学手工推导对照；
// 空窗策略（percentile_cont 空集 NULL → Go 零值结构，Source 恒标分支）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGStatsTTFTExactVsSeed 固定种子精确断言（spec §7.7）：
//
//	exact（usage_logs percentile_cont——连续插值 x = p×(n−1)，0 基）：
//	  account 7 / model m：[30×6, 60×4]（n=10）
//	    p50: x=4.5 → 30+0.5×0 = 30；p95: x=8.55 → 60；p99: x=8.91 → 60
//	    avg = (180+240)/10 = 42；max = 60
//	  account 7 / model other：[500]（n=1）→ 全 500（模型过滤隔离探针）
//	  account 8：[100×2, 200×2]（n=4）
//	    p50: x=1.5 → 100+0.5×100 = 150；p95/p99: x≥2.85 → 200；avg = 150
//	sketch（cube hist 合并 + nearest-rank 桶内线性插值，平台级无实体过滤）：
//	  全量 hist = [6,4,2,2,1,0...]（30×6→档0、60×4→档1、100×2→档2、200×2→档3、
//	  500→档4），N=15，total = 180+240+200+400+500 = 1520，max = 500：
//	    p50: rank=ceil(7.5)=8 → 档1：50+(8−6)/4×50 = 75
//	    p95: rank=ceil(14.25)=15 → 档4：400+(15−14)/1×400 = 800
//	    p99: rank=ceil(14.85)=15 → 800；avg = round(1520/15) = 101
func TestPGStatsTTFTExactVsSeed(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(time.Hour))

	ttft := func(v int64) *int64 { return &v }
	mk := func(rid string, accountID int64, model string, ms int64) *domain.UsageLog {
		return usageLogRow(rid, h.Add(time.Minute), domain.ErrNone, 1, accountID, 9, 5, model,
			1, 1, 0, 0, 0, 0, 0, ttft(ms))
	}
	logs := []*domain.UsageLog{
		mk("tf1", 7, "m", 30), mk("tf2", 7, "m", 30), mk("tf3", 7, "m", 30),
		mk("tf4", 7, "m", 30), mk("tf5", 7, "m", 30), mk("tf6", 7, "m", 30),
		mk("tf7", 7, "m", 60), mk("tf8", 7, "m", 60), mk("tf9", 7, "m", 60), mk("tf10", 7, "m", 60),
		mk("tf11", 7, "other", 500),
		mk("tf12", 8, "mA", 100), mk("tf13", 8, "mA", 100),
		mk("tf14", 8, "mB", 200), mk("tf15", 8, "mB", 200),
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))

	from, to := h, h.Add(time.Hour)

	// —— exact：account 7 全模型 ——
	s, err := repos.Stats.StatsTTFTExact(ctx, from, to, "account", 7, "")
	require.NoError(t, err)
	require.Equal(t, int64(11), s.Count)
	require.Equal(t, int64(84), s.AvgMS, "avg = round(920/11)")
	require.Equal(t, int64(30), s.P50MS, "percentile_cont 连续插值 x=5.0")
	require.Equal(t, int64(280), s.P95MS, "x=9.5 → 60+0.5×440")
	require.Equal(t, int64(456), s.P99MS, "x=9.9 → 60+0.9×440")
	require.Equal(t, int64(500), s.MaxMS)
	require.Equal(t, "exact", s.Source)

	// exact：model 过滤隔离（account 7 仅 model=m）
	s, err = repos.Stats.StatsTTFTExact(ctx, from, to, "account", 7, "m")
	require.NoError(t, err)
	require.Equal(t, int64(10), s.Count)
	require.Equal(t, int64(42), s.AvgMS)
	require.Equal(t, int64(30), s.P50MS)
	require.Equal(t, int64(60), s.P95MS)
	require.Equal(t, int64(60), s.P99MS)
	require.Equal(t, int64(60), s.MaxMS)

	// exact：account 8 已知分布
	s, err = repos.Stats.StatsTTFTExact(ctx, from, to, "account", 8, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), s.Count)
	require.Equal(t, int64(150), s.AvgMS)
	require.Equal(t, int64(150), s.P50MS, "x=1.5 → 100+0.5×100")
	require.Equal(t, int64(200), s.P95MS)
	require.Equal(t, int64(200), s.P99MS)

	// exact 空窗：零值结构 + Source 恒标分支（判空以 Count 为准）
	s, err = repos.Stats.StatsTTFTExact(ctx, from, to, "key", 999999, "")
	require.NoError(t, err)
	require.Zero(t, s.Count)
	require.Zero(t, s.P50MS)
	require.Equal(t, "exact", s.Source, "空窗 Source 恒标分支")

	// exact 白名单守门
	_, err = repos.Stats.StatsTTFTExact(ctx, from, to, "group", 1, "")
	require.Error(t, err)

	// —— sketch：先聚合落盘（同批数据全量入 cube），平台级合并对照 ——
	cube, _, _, err := repos.Stats.LoadAggRange(ctx, from, to)
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, from, to, to.Add(-time.Minute), cube, nil))

	s, err = repos.Stats.StatsTTFTSketch(ctx, from, to, "")
	require.NoError(t, err)
	require.Equal(t, int64(15), s.Count, "全平台样本数（含 account 7/8 全部行）")
	require.Equal(t, int64(101), s.AvgMS, "avg = round(1520/15)")
	require.Equal(t, []int64{6, 4, 2, 2, 1, 0, 0, 0, 0, 0}, cubeHistOf(t, repos, from, to), "cube hist 档位分布")
	require.Equal(t, int64(75), s.P50MS, "rank=8 → 档1：50+(8−6)/4×50")
	require.Equal(t, int64(800), s.P95MS, "rank=15 → 档4 [400,800)：400+1/1×400")
	require.Equal(t, int64(800), s.P99MS)
	require.Equal(t, int64(500), s.MaxMS)
	require.Equal(t, "sketch", s.Source)

	// sketch：model 过滤（仅 model=m 的桶——hist [6,4]，N=10，total=420）
	s, err = repos.Stats.StatsTTFTSketch(ctx, from, to, "m")
	require.NoError(t, err)
	require.Equal(t, int64(10), s.Count)
	require.Equal(t, int64(42), s.AvgMS)
	require.Equal(t, int64(41), s.P50MS, "rank=5 → 档0：0+5/6×50 = 41.67 截断为 41")

	// sketch 空窗：零值结构 + Source 恒标分支
	s, err = repos.Stats.StatsTTFTSketch(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), "")
	require.NoError(t, err)
	require.Zero(t, s.Count)
	require.Equal(t, "sketch", s.Source)
}

// cubeHistOf 读回窗口内合并直方图（断言辅助——锚定档位分布推导）。
func cubeHistOf(t *testing.T, repos *repository.Repository, from, to time.Time) []int64 {
	t.Helper()
	sum, err := repos.Stats.SummarizeStats(context.Background(), from, to, 0)
	require.NoError(t, err)
	return sum.TTFTHist
}
