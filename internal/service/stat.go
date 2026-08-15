// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// QueryStats 拉取小时桶并按 granularity（hour|day）在内存聚合。
func (s *Service) QueryStats(ctx context.Context, q repository.StatQuery, granularity string) ([]*domain.StatBucket, error) {
	rows, err := s.store.ScanStats(ctx, q)
	if err != nil {
		return nil, err
	}
	if granularity == "hour" || len(rows) == 0 {
		return rows, nil
	}
	// day 聚合：按 (日, 各维度) 合并（含 UserID——/user/stats 按用户过滤，
	// 不同用户的同维度桶不可合并）。日界固定 UTC（与 trend SQL date_trunc('day',
	// bucket_time AT TIME ZONE 'UTC') 同语义——评审 P2-1；本处曾用会话时区本地日
	// Format，跨本地日界的相邻小时桶（如 23:00/00:00 +0800）被拆成两日，漏合并）。
	merged := make(map[string]*domain.StatBucket)
	for _, b := range rows {
		key := b.BucketTime.UTC().Format("2006-01-02") + "|" + itoa(b.GroupID) + "|" + itoa(b.AccountID) + "|" + itoa(b.TemplateID) + "|" + itoa(b.UserID) + "|" + b.Model + "|" + boolStr(b.IsError)
		m, ok := merged[key]
		if !ok {
			day := b.BucketTime.Truncate(24 * time.Hour)
			m = &domain.StatBucket{
				BucketTime: day, GroupID: b.GroupID, AccountID: b.AccountID,
				TemplateID: b.TemplateID, UserID: b.UserID, Model: b.Model, IsError: b.IsError,
			}
			merged[key] = m
		}
		m.RequestCount += b.RequestCount
		m.ErrorCount += b.ErrorCount
		m.InputTokens += b.InputTokens
		m.OutputTokens += b.OutputTokens
		m.TotalTokens += b.TotalTokens
		m.CacheReadTokens += b.CacheReadTokens
		m.CacheCreationTokens += b.CacheCreationTokens
		m.Cost += b.Cost
		m.CallCount += b.CallCount // 按次调用（spec 2026-08-14：入桶与展示）
		// TTFT 四字段日合并（rewrite spec 2026-08-14 前置清单②）：total/count
		// 求和、max 取大、直方图逐元素加（与 repository.mergeHist 同语义——
		// 行数 ≤ 24h × 维度，元素级加法 O(10) 无性能面）。
		m.TTFTTotalMS += b.TTFTTotalMS
		m.TTFTCount += b.TTFTCount
		if b.TTFTMaxMS > m.TTFTMaxMS {
			m.TTFTMaxMS = b.TTFTMaxMS
		}
		if len(b.TTFTHist) > 0 {
			if m.TTFTHist == nil {
				m.TTFTHist = make([]int64, len(b.TTFTHist))
			}
			for i, c := range b.TTFTHist {
				if i < len(m.TTFTHist) {
					m.TTFTHist[i] += c
				}
			}
		}
	}
	out := make([]*domain.StatBucket, 0, len(merged))
	for _, m := range merged {
		out = append(out, m)
	}
	return out, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
