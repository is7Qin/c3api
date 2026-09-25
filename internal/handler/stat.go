// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"math"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// GetStatsTrend 趋势聚合（按请求 `timezone` 判定 cube/原始行走哪条——判定在
// domain.Admit，本层只读判定结果）。200 体是裸数组，故生效窗口与实际存储走
// 回显头（spec §4.4(b)）。ServerInterface。
func (h *AdminAPI) GetStatsTrend(w http.ResponseWriter, r *http.Request, params GetStatsTrendParams) {
	zone, err := resolveStatsZone(params.Timezone)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	// 非法 granularity 直透 service 哨兵→400（handler 静默回落
	// 会掩盖 normalizeGranularity 的 ErrInvalidInput，契约不一致）。
	granularity := ""
	if params.Granularity != nil {
		granularity = string(*params.Granularity)
	}
	q := service.TrendQuery{
		From:        params.From,
		To:          params.To,
		Granularity: granularity,
		Zone:        zone,
	}
	if params.GroupId != nil {
		q.GroupID = *params.GroupId
	}
	if params.Model != nil {
		q.Model = *params.Model
	}
	res, err := h.svc.QueryStatsTrend(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteStatsEcho(w, res.Exec)
	out := make([]StatTrendPoint, 0, len(res.Buckets))
	for _, b := range res.Buckets {
		out = append(out, toAPIStatTrendPoint(b))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// GetStatsTop Top 排行（entity 卷积）。排行按实体聚合、无时间桶——数值与
// 时区无关；`timezone` 参数仅接受并校验（契约一致性：客户端统一带浏览器
// 时区，非法名照旧 400），不进查询。
func (h *AdminAPI) GetStatsTop(w http.ResponseWriter, r *http.Request, params GetStatsTopParams) {
	if _, err := resolveStatsZone(params.Timezone); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	limit := 20
	if params.Limit != nil {
		limit = *params.Limit
	}
	limit = httpface.ClampLimit(limit)
	if limit <= 0 {
		limit = 20
	}
	q := service.TopQuery{
		From:       params.From,
		To:         params.To,
		EntityType: string(params.Entity),
		By:         string(params.By),
		Limit:      limit,
	}
	rows, err := h.svc.QueryStatsTop(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]StatTopEntry, 0, len(rows))
	for _, b := range rows {
		out = append(out, toAPIStatTopEntry(b))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// GetStatsEntityTrend 实体趋势（时区判定/回显同 GetStatsTrend）。
func (h *AdminAPI) GetStatsEntityTrend(w http.ResponseWriter, r *http.Request, params GetStatsEntityTrendParams) {
	zone, err := resolveStatsZone(params.Timezone)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q := service.EntityTrendQuery{
		EntityType:  string(params.Entity),
		EntityID:    params.Id,
		From:        params.From,
		To:          params.To,
		Granularity: string(params.Granularity),
		Zone:        zone,
	}
	if params.Model != nil {
		q.Model = *params.Model
	}
	res, err := h.svc.QueryEntityTrend(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteStatsEcho(w, res.Exec)
	out := make([]StatTrendPoint, 0, len(res.Buckets))
	for _, b := range res.Buckets {
		out = append(out, toAPIEntityStatTrendPoint(b))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// GetStatsTTFT TTFT 聚合（sketch/exact 双分支）。分位数/计数为绝对区间数值
// ——时区不改变结果；`timezone` 仅接受并校验（非法 400），不进查询、不进缓存键。
func (h *AdminAPI) GetStatsTTFT(w http.ResponseWriter, r *http.Request, params GetStatsTTFTParams) {
	if _, err := resolveStatsZone(params.Timezone); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q := service.TTFTQuery{
		From: params.From,
		To:   params.To,
	}
	if params.Entity != nil {
		q.EntityType = string(*params.Entity)
	}
	if params.Id != nil {
		q.EntityID = *params.Id
	}
	if params.Model != nil {
		q.Model = *params.Model
	}
	sum, err := h.svc.QueryStatsTTFT(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIStatTTFTSummary(sum))
}

// GetStatsCapabilities 统计能力（spec §4.4(a)）：本部署的 per-deployment 常量，
// 无参数、只读、可按部署缓存。**零字面量**——数据面由
// `service.StatsCapabilities`（= domain.StatsKinds × 注入保留期的机械投影）给出，
// 本层只做形状映射（改这里的数字不可能，因为没有任何数字可改：上限/覆盖天数
// 全部来自同一份 KIND 矩阵）。`zero literals` 由 spec §8 A16'② 的
// 零命中 `git grep` 审计钉死：本包（含测试）不得出现任何上限秒值字面量。
func (h *AdminAPI) GetStatsCapabilities(w http.ResponseWriter, r *http.Request) {
	httpface.WriteJSON(w, http.StatusOK, toAPIStatsCapabilities(h.svc.StatsCapabilities()))
}

// toAPIStatsCapabilities domain 投影 → 线缆类型（逐字段搬运，无数值字面量）。
func toAPIStatsCapabilities(c domain.Capabilities) StatsCapabilities {
	kinds := make(map[string]StatsKindCapability, len(c.Kinds))
	for id, k := range c.Kinds {
		storages := make([]StatsKindCapabilityStorages, 0, len(k.Storages))
		caps := make(map[string]int64, len(k.Storages))
		days := make(map[string]int, len(k.Storages))
		for _, s := range k.Storages {
			name := s.String()
			storages = append(storages, StatsKindCapabilityStorages(name))
			caps[name] = k.CostCapSeconds[s]
			days[name] = k.CoverageDays[s]
		}
		kinds[string(id)] = StatsKindCapability{
			Grouping:       k.Grouping.String(),
			Storages:       storages,
			CostCapSeconds: caps,
			CoverageDays:   days,
		}
	}
	return StatsCapabilities{BucketGridSeconds: int(c.BucketGridSeconds), Kinds: kinds}
}

func toAPIStatTrendPoint(b *domain.StatBucket) StatTrendPoint {
	var avg float64
	if b.TTFTCount > 0 {
		avg = math.Round(float64(b.TTFTTotalMS) / float64(b.TTFTCount))
	}
	return StatTrendPoint{
		BucketTime:          &b.BucketTime,
		RequestCount:        &b.RequestCount,
		ErrorCount:          &b.ErrorCount,
		CallCount:           &b.CallCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                ptr(millisToUSD(b.Cost)),
		RawCost:             ptr(millisToUSD(b.RawCost)),
		TTFTAvgMS:           ptr(avg),
		TTFTMaxMS:           &b.TTFTMaxMS,
	}
}

func toAPIEntityStatTrendPoint(b *domain.EntityStatBucket) StatTrendPoint {
	var avg float64
	if b.TTFTCount > 0 {
		avg = math.Round(float64(b.TTFTTotalMS) / float64(b.TTFTCount))
	}
	return StatTrendPoint{
		BucketTime:          &b.BucketTime,
		RequestCount:        &b.RequestCount,
		ErrorCount:          &b.ErrorCount,
		CallCount:           &b.CallCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                ptr(millisToUSD(b.Cost)),
		RawCost:             ptr(millisToUSD(b.RawCost)),
		TTFTAvgMS:           ptr(avg),
		TTFTMaxMS:           &b.TTFTMaxMS,
	}
}

func toAPIStatTopEntry(b *domain.EntityStatBucket) StatTopEntry {
	var avg float64
	if b.TTFTCount > 0 {
		avg = math.Round(float64(b.TTFTTotalMS) / float64(b.TTFTCount))
	}
	et := StatTopEntryEntityType(b.EntityType)
	return StatTopEntry{
		EntityType:          &et,
		EntityID:            &b.EntityID,
		RequestCount:        &b.RequestCount,
		ErrorCount:          &b.ErrorCount,
		CallCount:           &b.CallCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                ptr(millisToUSD(b.Cost)),
		RawCost:             ptr(millisToUSD(b.RawCost)),
		TTFTAvgMS:           ptr(avg),
		TTFTMaxMS:           &b.TTFTMaxMS,
	}
}

func toAPIStatTTFTSummary(s *domain.TTFTSummary) StatTTFTSummary {
	if s == nil {
		s = &domain.TTFTSummary{Source: "sketch"}
	}
	src := StatTTFTSummarySource(s.Source)
	return StatTTFTSummary{
		Count:  s.Count,
		AvgMS:  float64(s.AvgMS),
		P50MS:  s.P50MS,
		P95MS:  s.P95MS,
		P99MS:  s.P99MS,
		MaxMS:  s.MaxMS,
		Source: src,
	}
}
