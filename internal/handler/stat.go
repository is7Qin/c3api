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

// GetStatsTrend 趋势聚合（cube 或原始行——按请求 `timezone` 由 repo 路由）。
// ServerInterface。
func (h *AdminAPI) GetStatsTrend(w http.ResponseWriter, r *http.Request, params GetStatsTrendParams) {
	zone, err := resolveStatsZone(params.Timezone)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	// 非法 granularity 直透 service 哨兵→400（RG-BE M-1：handler 静默回落
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
	rows, err := h.svc.QueryStatsTrend(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]StatTrendPoint, 0, len(rows))
	for _, b := range rows {
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

// GetStatsEntityTrend 实体趋势（时区路由同 GetStatsTrend）。
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
	rows, err := h.svc.QueryEntityTrend(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]StatTrendPoint, 0, len(rows))
	for _, b := range rows {
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
