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

// GetStatsTrend 趋势聚合（cube）— ServerInterface。
func (h *AdminAPI) GetStatsTrend(w http.ResponseWriter, r *http.Request, params GetStatsTrendParams) {
	granularity := "day"
	if params.Granularity != nil {
		granularity = string(*params.Granularity)
		if granularity != "hour" && granularity != "day" {
			granularity = "day"
		}
	}
	q := service.TrendQuery{
		From:        params.From,
		To:          params.To,
		Granularity: granularity,
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

// GetStatsTop Top 排行（entity 卷积）。
func (h *AdminAPI) GetStatsTop(w http.ResponseWriter, r *http.Request, params GetStatsTopParams) {
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

// GetStatsEntityTrend 实体趋势。
func (h *AdminAPI) GetStatsEntityTrend(w http.ResponseWriter, r *http.Request, params GetStatsEntityTrendParams) {
	q := service.EntityTrendQuery{
		EntityType:  string(params.Entity),
		EntityID:    params.Id,
		From:        params.From,
		To:          params.To,
		Granularity: string(params.Granularity),
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

// GetStatsTTFT TTFT 聚合（sketch/exact 双分支）。
func (h *AdminAPI) GetStatsTTFT(w http.ResponseWriter, r *http.Request, params GetStatsTTFTParams) {
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
