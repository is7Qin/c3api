// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// quality-cost frontier（service lane）：rollup 质量行 × 当前发布计划
// 候选目录的连接视图。数学全部复用 scheduler 既有核（Wilson95 /
// LogTTFTInterval / IsExplore / AvgTokens / SaturatingMulDiv）与 billing
// 纯函数——本文件不新发明任何统计公式。

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// q32Scale TTFT 对数和定点缩放（与 quality recorder 写侧同量）。
const q32Scale = float64(int64(1) << 32)

// RoutingFrontierQuery frontier 入参。Limit ≤0 → 200；>200 钳到 200
// （MaxStatsListLimit 同值同语义）。
type RoutingFrontierQuery struct {
	RouteID string
	From    time.Time
	To      time.Time
	Limit   int
}

// RoutingFrontierCandidate 一个候选指纹的窗口聚合。Known = 指纹在当前发布
// 计划该路由的候选目录内（join 键 = IdentityFingerprint，compiler 合成规则
// 同源）；未知候选只报告观测事实，不参与支配排序。Insufficient = 样本 <30
// （scheduler.IsExplore 同一门槛）。CostKnown 需 known + 有成功样本 + 价格
// 可解析（与 compiler cost 门语义一致）。
type RoutingFrontierCandidate struct {
	CandidateFingerprint string
	Known                bool
	AccountID            int64
	TemplateID           int64
	// IdentityRevision 候选内容代际 K（编译器事实与 wire 同源）。注意：这不是
	// 客户端 CAS 令牌 C（lifecycle_revision）——C 只围栏管理员写入，与候选
	// 内容身份无关，不得在此暴露为「代际」。
	IdentityRevision int64
	QualityClassID   string
	MappedModel      string
	Attempts         int64
	Successes        int64
	SuccessLCB       float64
	SuccessUCB       float64
	TTFTLCB          float64
	TTFTUCB          float64
	TTFTKnown        bool
	CostPerSuccess   int64
	CostKnown        bool
	Insufficient     bool
	OnFrontier       bool
}

// RoutingFrontierResult frontier 查询结果（候选已排序 + 钳制）。
type RoutingFrontierResult struct {
	RouteClassID   string
	PlanGeneration uint64
	Candidates     []RoutingFrontierCandidate
}

// QueryRoutingFrontier 一条路由类的质量-成本前沿。支配只在 known &&
// CostKnown 候选之间进行（LCB 越高越好、成本越低越好）；unknown/无成本/
// 样本不足者如实呈现但不上前沿。输出确定性排序：前沿优先 → LCB 降序 →
// 成本升序 → 指纹升序，随后钳到 limit。
func (s *Service) QueryRoutingFrontier(ctx context.Context, q RoutingFrontierQuery) (*RoutingFrontierResult, error) {
	if err := validateStatsWindow(q.From, q.To, MaxStatsTrendSpan); err != nil {
		return nil, err
	}
	plan, route, rc, err := s.resolveRoutingRoute(q.RouteID)
	if err != nil {
		return nil, err
	}
	reader, ok := s.store.(RoutingRollupReader)
	if !ok {
		return nil, errRoutingNotWired
	}
	limit := q.Limit
	if limit <= 0 {
		limit = MaxStatsListLimit
	}
	limit = min(limit, MaxStatsListLimit)
	rows, err := reader.QueryQualityRollupStats(ctx, rc, int16(domain.RoutingIdentityVersion), q.From, q.To)
	if err != nil {
		return nil, err
	}
	byFP := make(map[string]scheduler.RoutingPlanCandidate, len(route.Candidates))
	for _, c := range route.Candidates {
		byFP[c.IdentityFingerprint] = c
	}
	now := time.Now()
	cands := make([]RoutingFrontierCandidate, 0, len(rows))
	for _, row := range rows {
		c := RoutingFrontierCandidate{
			CandidateFingerprint: domain.CandidateFPHex(row.CandidateFingerprint),
			Attempts:             row.Attempts,
			Successes:            row.Successes,
		}
		interval := scheduler.Wilson95(int(row.Successes), int(row.Attempts))
		c.SuccessLCB, c.SuccessUCB = interval.LCB, interval.UCB
		ttft, ttftOK := scheduler.LogTTFTInterval(
			float64(row.TTFTSumLogQ32)/q32Scale, float64(row.TTFTSumSqLogQ32)/q32Scale, int(row.TTFTN))
		c.TTFTKnown = ttftOK
		c.TTFTLCB, c.TTFTUCB = ttft.LCB, ttft.UCB
		c.Insufficient = scheduler.IsExplore(int(row.Attempts))
		if planCand, known := byFP[c.CandidateFingerprint]; known {
			c.Known = true
			c.AccountID = planCand.AccountID
			c.TemplateID = planCand.TemplateID
			c.IdentityRevision = planCand.IdentityRevision
			c.QualityClassID = planCand.QualityClassID
			c.MappedModel = planCand.MappedModel
			c.CostPerSuccess, c.CostKnown = s.frontierCost(route.Ref.Model, row, planCand.UpstreamCostMultiplierBp, now)
		}
		cands = append(cands, c)
	}
	markParetoFrontier(cands)
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.OnFrontier != b.OnFrontier {
			return a.OnFrontier
		}
		if a.SuccessLCB != b.SuccessLCB {
			return a.SuccessLCB > b.SuccessLCB
		}
		if ac, bc := frontierSortCost(a), frontierSortCost(b); ac != bc {
			return ac < bc
		}
		return a.CandidateFingerprint < b.CandidateFingerprint
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	return &RoutingFrontierResult{
		RouteClassID:   q.RouteID,
		PlanGeneration: plan.Generation,
		Candidates:     cands,
	}, nil
}

// frontierCost 每次成功平均成本（compiler 同式：avg tokens/success ×
// 请求模型解析价 × 采购倍率 bp/10000，饱和乘除）。价格不可解析或无成功
// 样本 → costKnown=false（与 compiler 的 costKnown 门一致）。
func (s *Service) frontierCost(requestedModel string, row repository.RoutingQualityStat, multBp int, at time.Time) (int64, bool) {
	if row.Successes <= 0 {
		return 0, false
	}
	avgIn := scheduler.AvgTokens(row.InputTokens, row.Successes)
	price, hasPrice := s.ResolvePrices(requestedModel, avgIn, "", at)
	if !hasPrice {
		return 0, false
	}
	raw := billing.CostFromResolved(price,
		avgIn,
		scheduler.AvgTokens(row.OutputTokens, row.Successes),
		scheduler.AvgTokens(row.CacheReadTokens, row.Successes),
		scheduler.AvgTokens(row.CacheCreateTokens, row.Successes))
	mult := max(min(multBp, 100000), 0)
	cost := scheduler.SaturatingMulDiv(raw, int64(mult), 10000)
	if cost < 0 {
		cost = 0
	}
	return cost, true
}

// frontierSortCost 排序键：成本未知者排最后（MaxInt64 哨兵，不参与支配）。
func frontierSortCost(c RoutingFrontierCandidate) int64 {
	if c.CostKnown {
		return c.CostPerSuccess
	}
	return math.MaxInt64
}

// markParetoFrontier 在 known && CostKnown 集合内标记非支配点：按成本升序
// （同成本按 LCB 降序）扫描，LCB 严格超过历史最优者上前沿——同 (成本, LCB)
// 重复点只保留首个（确定性）。其余候选 OnFrontier=false。
// ponytail: O(n log n) 二维 Pareto 扫描，n = 单路由类候选数（≤ 账号数），
// 若未来需要三维（含 TTFT）再换结构。
func markParetoFrontier(cands []RoutingFrontierCandidate) {
	idx := make([]int, 0, len(cands))
	for i := range cands {
		if cands[i].Known && cands[i].CostKnown {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ca, cb := cands[idx[a]], cands[idx[b]]
		if ca.CostPerSuccess != cb.CostPerSuccess {
			return ca.CostPerSuccess < cb.CostPerSuccess
		}
		return ca.SuccessLCB > cb.SuccessLCB
	})
	bestLCB := math.Inf(-1)
	for _, i := range idx {
		if cands[i].SuccessLCB > bestLCB {
			cands[i].OnFrontier = true
			bestLCB = cands[i].SuccessLCB
		}
	}
}
