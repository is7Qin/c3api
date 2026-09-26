// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// —— /api/admin/overview 聚合面（spec 2026-08-14；全冷面：聚合查询 + 快照遍历，
// 门禁/计费面零改动） ——

// OverviewAccounts 账号健康分布 + 并发水位（调度器快照同源——与账号列表
// 运行时视图 ListAccountViews 一致：状态取快照 EWMA 状态，并发/水位取快照
// 原子计数器）。
type OverviewAccounts struct {
	Active         int
	Unhealthy      int
	N429           int
	Disabled       int
	Concurrency    int64
	MaxConcurrency int64
}

// OverviewErrTop 账号维度错误率条目（err_top；name = 账号名）。
type OverviewErrTop struct {
	Name     string
	ErrRate  float64
	ErrCount int
}

// OverviewData 总览聚合结果（内部单位：cost 毫分——USD 换算在 handler 边界
// /1e5，与价格 API 口径一致）。
type OverviewData struct {
	Summary   repository.StatSummary
	Trend     []*repository.StatDayAgg
	Accounts  OverviewAccounts
	Resources repository.OverviewResourceCounts
	ErrTop    []OverviewErrTop
}

// Overview 管理端总览聚合（/api/admin/overview 服务端聚合面）：
//
//	summary = [day, day+1本地日) 区间单行 sum（SQL 侧）；
//	trend   = [day−(days−1)本地日, day+1本地日) 日桶（SQL 侧按请求时区日界
//	          分组——恒整点无 DST 时区走 usage_stats cube 重组（分区键 range
//	          毫秒级）；DST/半小时时区走原始行精确聚合，路由由 domain.Admit
//	          判定后在本方法选 Cube/Raw）；
//	accounts/err_top = 调度器快照遍历（O(N) 冷面，30s 缓存摊薄）；
//	resources = 三表冷面 count。
//
// day 由调用方传入（handler 缓存键与聚合区间同一日界源——请求浏览器时区本地
// 日零点，跨午夜滚转不漂移）；日窗推进用日历 AddDate（DST 安全，绝不用固定
// 24h 算术）；zone = handler 边界解析过的请求时区（nil/UTC = 现状 cube 路径，
// 向后兼容）。**两个窗口各自过一次 domain.Admit**（summary 用 [day,to)、trend
// 用 [from,to)——同一 kind 矩阵判定，cost/coverage 对两个 Exec 分别生效）；
// days 已由调用方钳制 [1,30]；groupID > 0 = 按组过滤 summary/trend
// （accounts/err_top/resources 为全局面，spec 参数语义）。
//
// 可证结论（spec §4.5）：Overview 的两个计划**永远不会是 WindowShifted**——
// 整点无 DST 时区下 day/from/to 都是本地零点即 UTC 整点 ⇒ 第一条判定命中；非
// 整点偏移或跨 DST 时区下对齐后的 cand 仍不精确 ⇒ 第三条判定。故 overview
// 无需窗口回显（P2）。
func (s *Service) Overview(ctx context.Context, day time.Time, days int, groupID int64, zone *time.Location) (*OverviewData, error) {
	from := day.AddDate(0, 0, -(days - 1))
	to := day.AddDate(0, 0, 1)
	sumExec, err := s.admitStats(domain.KindSummary, zone, day, to)
	if err != nil {
		return nil, err
	}
	daysExec, err := s.admitStats(domain.KindDays, zone, from, to)
	if err != nil {
		return nil, err
	}
	var summary *repository.StatSummary
	if sumExec.Storage == domain.StatsStorageCube {
		summary, err = s.store.SummarizeStatsCube(ctx, sumExec.From, sumExec.To, groupID, sumExec.Zone)
	} else {
		summary, err = s.store.SummarizeStatsRaw(ctx, sumExec.From, sumExec.To, groupID, sumExec.Zone)
	}
	if err != nil {
		return nil, err
	}
	var trend []*repository.StatDayAgg
	if daysExec.Storage == domain.StatsStorageCube {
		trend, err = s.store.ScanStatsDaysCube(ctx, daysExec.From, daysExec.To, groupID, daysExec.Zone)
	} else {
		trend, err = s.store.ScanStatsDaysRaw(ctx, daysExec.From, daysExec.To, groupID, daysExec.Zone)
	}
	if err != nil {
		return nil, err
	}
	res, err := s.store.CountOverviewResources(ctx)
	if err != nil {
		return nil, err
	}
	var acc OverviewAccounts
	var errTop []OverviewErrTop
	if s.sched != nil {
		for _, rt := range s.sched.Runtimes() {
			switch rt.Status {
			case domain.StatusActive:
				acc.Active++
			case domain.StatusUnhealthy:
				acc.Unhealthy++
			case domain.Status429:
				acc.N429++
			case domain.StatusDisabled:
				acc.Disabled++
			}
			acc.Concurrency += rt.Concurrency
			acc.MaxConcurrency += int64(rt.MaxConcurrency)
			if rt.ErrRate > 0 {
				errTop = append(errTop, OverviewErrTop{Name: rt.Name, ErrRate: rt.ErrRate, ErrCount: rt.ErrCount})
			}
		}
	}
	// err_top 排序（与 dashboard 现有同源聚合一致：err_rate 降序 Top5；
	// 同率按 err_count 降序、名字升序兜底确定性）。
	slices.SortFunc(errTop, func(a, b OverviewErrTop) int {
		if a.ErrRate != b.ErrRate {
			if a.ErrRate < b.ErrRate {
				return 1
			}
			return -1
		}
		if a.ErrCount != b.ErrCount {
			return b.ErrCount - a.ErrCount
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(errTop) > 5 {
		errTop = errTop[:5]
	}
	return &OverviewData{
		Summary:   *summary,
		Trend:     trend,
		Accounts:  acc,
		Resources: *res,
		ErrTop:    errTop,
	}, nil
}

// UserEmails 批量取邮箱（/api/admin/users-top TopN 回填；users 表无 name 列——
// 仅 email；id IN 一次查询）。缺失 id 不在 map（handler 兜底空串）。
func (s *Service) UserEmails(ctx context.Context, ids []int64) (map[int64]string, error) {
	return s.store.ListUserEmails(ctx, ids)
}
