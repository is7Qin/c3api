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
//	summary = [utcDay, utcDay+1d) 区间单行 sum（SQL 侧）；
//	trend   = [utcDay-(days-1)d, utcDay+1d) 日桶（SQL 侧 GROUP BY
//	          date_trunc('day', bucket_time)——usage_stats 分区键 range 毫秒级）；
//	accounts/err_top = 调度器快照遍历（O(N) 冷面，30s 缓存摊薄）；
//	resources = 三表冷面 count。
//
// utcDay 由调用方传入（handler 缓存键与聚合区间同一日界源——跨 UTC 午夜
// 滚转不漂移）；days 已由调用方钳制 [1,30]；groupID > 0 = 按组过滤
// summary/trend（accounts/err_top/resources 为全局面，spec 参数语义）。
func (s *Service) Overview(ctx context.Context, utcDay time.Time, days int, groupID int64) (*OverviewData, error) {
	from := utcDay.Add(-time.Duration(days-1) * 24 * time.Hour)
	to := utcDay.Add(24 * time.Hour)
	summary, err := s.store.SummarizeStats(ctx, utcDay, to, groupID)
	if err != nil {
		return nil, err
	}
	trend, err := s.store.ScanStatsDays(ctx, from, to, groupID)
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
