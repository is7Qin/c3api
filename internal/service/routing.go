// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 路由观测查询面（Todo 17 service lane）：routing flow / quality-cost frontier /
// plan explanation 三个只读聚合入口。数据源钉死为 rollup 表（repository 聚合读）
// 与 scheduler 当前发布计划的防御性投影——绝不查 usage_logs/err_logs，绝不在
// 查询路径重编译/重过滤。丢失计数（incomplete/overflow/crash-unobservable）
// 只来自本进程 quality 观测面，与上游失败语义严格分离。

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// RoutingPlanProvider 当前发布计划投影能力（实现 = *scheduler.Scheduler，
// 经 s.sched 能力探测——与 RuntimeProvider 同一注入实例，不新增装配参数）。
type RoutingPlanProvider interface {
	CurrentRoutingPlan() *scheduler.RoutingPlan
}

// RoutingRollupReader rollup 聚合读能力（实现 = *repository.Repository 对
// Partitions 的委托，经 s.store 能力探测）。
type RoutingRollupReader interface {
	QueryQualityRollupStats(ctx context.Context, routeClass domain.RouteClassIDVal, identityVersion int16, from, to time.Time) ([]repository.RoutingQualityStat, error)
	QueryFlowRollupStats(ctx context.Context, routeClass domain.RouteClassIDVal, identityVersion int16, from, to time.Time) ([]repository.RoutingFlowStat, error)
}

// routingLoss 进程观测 flow 丢失计数接缝（quality 包级原子计数器的读取面；
// 测试替换函数字段注入确定值）。incomplete = 本进程已观察 cleanup 缺 terminal；
// overflow = 故障预算淘汰链（容量拒绝 + 入队预算拒绝）。
var routingLoss = struct {
	incomplete func() int64
	overflow   func() int64
}{
	incomplete: quality.FlowChainIncompleteObserved,
	overflow:   func() int64 { return quality.FlowChainCapacityOverflow() + quality.FlowChainEnqueueOverflow() },
}

var errRoutingNotWired = errors.New("service: routing observation surface not wired")

// RoutingPlanExplanation 当前发布计划的只读投影（防御性拷贝，调用方可任意
// 持有/排序/序列化）。空视图 = Generation 0 空计划，不是错误。
func (s *Service) RoutingPlanExplanation() (*scheduler.RoutingPlan, error) {
	p, ok := s.sched.(RoutingPlanProvider)
	if !ok || p == nil {
		return nil, errRoutingNotWired
	}
	return p.CurrentRoutingPlan(), nil
}

// resolveRoutingRoute 校验 64-hex 路由类 ID 并对照当前发布计划目录：
// 非法 hex → ErrInvalidInput；目录缺失（未发布视图/未知路由）→ ErrNotFound。
// 返回的 plan/route 均为投影拷贝内的稳定值。
func (s *Service) resolveRoutingRoute(routeID string) (*scheduler.RoutingPlan, *scheduler.RoutingPlanRoute, domain.RouteClassIDVal, error) {
	raw, err := domain.HexToID(routeID)
	if err != nil {
		return nil, nil, domain.RouteClassIDVal{}, ErrInvalidInput
	}
	plan, err := s.RoutingPlanExplanation()
	if err != nil {
		return nil, nil, domain.RouteClassIDVal{}, err
	}
	for i := range plan.Routes {
		if plan.Routes[i].Ref.RouteClassID == routeID {
			return plan, &plan.Routes[i], domain.RouteClassIDVal(raw), nil
		}
	}
	return nil, nil, domain.RouteClassIDVal{}, ErrNotFound
}

// RoutingFlowQuery routing-flow 入参（窗口 ≤90d 精确上限，复用
// MaxStatsTrendSpan 常量与 validateStatsWindow 校验序）。
type RoutingFlowQuery struct {
	RouteID string // 64-hex route class ID（当前发布目录内）
	From    time.Time
	To      time.Time
}

// RoutingFlowEdge 一条聚合边（rollup 行的防御性拷贝；fingerprint 为 hex）。
type RoutingFlowEdge struct {
	Ordinal              int16
	Lane                 string
	AccountID            int64
	PreviousAccountID    *int64
	PreviousOutcome      string
	TransitionReason     string
	Outcome              string
	IsTerminal           bool
	Generation           int64
	CandidateFingerprint string
	ChainCount           int64
}

// RoutingFlowLane (ordinal, lane) 分组——retry 到下一 ordinal，terminal 边
// 即该链 Final。
type RoutingFlowLane struct {
	Ordinal int16
	Lane    string
	Edges   []RoutingFlowEdge
}

// RoutingFlowResult flow 聚合结果。守恒：完整链才入 rollup，故
// FirstDispatchChains（ordinal=1 链数和 = Attempt1）恒等于 TerminalChains
// （is_terminal 链数和）；三个丢失计数是独立观测口径，不得混入边/结局语义。
type RoutingFlowResult struct {
	RouteClassID                 string
	PlanGeneration               uint64
	Lanes                        []RoutingFlowLane
	FirstDispatchChains          int64
	TerminalChains               int64
	IncompleteChainDropped       int64
	FlowOverflowDroppedChains    int64
	ProcessCrashLossUnobservable bool
}

// QueryRoutingFlow 按 terminal_at 归属窗口查询一条路由类的完整链边聚合。
// 行序沿用 repository 确定性排序（ordinal, lane, account, prev NULLS FIRST,
// outcome…），lane 分组保持组内原序、组间按 (ordinal, lane) 全序。
func (s *Service) QueryRoutingFlow(ctx context.Context, q RoutingFlowQuery) (*RoutingFlowResult, error) {
	if err := validateStatsWindow(q.From, q.To, MaxStatsTrendSpan); err != nil {
		return nil, err
	}
	plan, _, rc, err := s.resolveRoutingRoute(q.RouteID)
	if err != nil {
		return nil, err
	}
	reader, ok := s.store.(RoutingRollupReader)
	if !ok {
		return nil, errRoutingNotWired
	}
	rows, err := reader.QueryFlowRollupStats(ctx, rc, int16(domain.RoutingIdentityVersion), q.From, q.To)
	if err != nil {
		return nil, err
	}
	res := &RoutingFlowResult{
		RouteClassID:                 q.RouteID,
		PlanGeneration:               plan.Generation,
		Lanes:                        []RoutingFlowLane{},
		IncompleteChainDropped:       routingLoss.incomplete(),
		FlowOverflowDroppedChains:    routingLoss.overflow(),
		ProcessCrashLossUnobservable: quality.ProcessCrashLossUnobservable(),
	}
	edges := make([]RoutingFlowEdge, 0, len(rows))
	for _, row := range rows {
		prev := row.PreviousAccountID
		edge := RoutingFlowEdge{
			Ordinal:              row.Ordinal,
			Lane:                 row.Lane,
			AccountID:            row.AccountID,
			PreviousOutcome:      row.PreviousOutcome,
			TransitionReason:     row.TransitionReason,
			Outcome:              row.Outcome,
			IsTerminal:           row.IsTerminal,
			Generation:           row.Generation,
			CandidateFingerprint: domain.CandidateFPHex(row.CandidateFingerprint),
			ChainCount:           row.ChainCount,
		}
		if prev != nil {
			v := *prev
			edge.PreviousAccountID = &v
		}
		if row.Ordinal == 1 {
			res.FirstDispatchChains += row.ChainCount
		}
		if row.IsTerminal {
			res.TerminalChains += row.ChainCount
		}
		edges = append(edges, edge)
	}
	// 组间 (ordinal, lane) 全序；组内保持 repository 确定性行序（stable）。
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Ordinal != edges[j].Ordinal {
			return edges[i].Ordinal < edges[j].Ordinal
		}
		return edges[i].Lane < edges[j].Lane
	})
	for _, e := range edges {
		if n := len(res.Lanes); n > 0 && res.Lanes[n-1].Ordinal == e.Ordinal && res.Lanes[n-1].Lane == e.Lane {
			res.Lanes[n-1].Edges = append(res.Lanes[n-1].Edges, e)
			continue
		}
		res.Lanes = append(res.Lanes, RoutingFlowLane{Ordinal: e.Ordinal, Lane: e.Lane, Edges: []RoutingFlowEdge{e}})
	}
	return res, nil
}
