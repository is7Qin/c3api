// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// Todo 17 service lane：routing flow / frontier / plan explanation 聚合查询的
// 单元测试。rollup 行由 fakeStore 供给（不触 PG），计划目录由 fake provider
// 供给（不触 compiler），丢失计数经 routingLoss 接缝注入确定值。

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- fakeStore rollup 面 ---

func (f *fakeStore) QueryQualityRollupStats(_ context.Context, routeClass domain.RouteClassIDVal, version int16, from, to time.Time) ([]repository.RoutingQualityStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routingRollupCall = struct {
		routeClass domain.RouteClassIDVal
		version    int16
		from, to   time.Time
	}{routeClass, version, from, to}
	if f.routingRollupErr != nil {
		return nil, f.routingRollupErr
	}
	return f.routingQualityRows, nil
}

func (f *fakeStore) QueryFlowRollupStats(_ context.Context, routeClass domain.RouteClassIDVal, version int16, from, to time.Time) ([]repository.RoutingFlowStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routingRollupCall = struct {
		routeClass domain.RouteClassIDVal
		version    int16
		from, to   time.Time
	}{routeClass, version, from, to}
	if f.routingRollupErr != nil {
		return nil, f.routingRollupErr
	}
	return f.routingFlowRows, nil
}

// --- fixtures ---

// fakeRoutingSched 只提供当前计划投影（RuntimeProvider 面留空）。
type fakeRoutingSched struct{ plan *scheduler.RoutingPlan }

func (f *fakeRoutingSched) Runtime(int64) (scheduler.RuntimeInfo, bool) {
	return scheduler.RuntimeInfo{}, false
}
func (f *fakeRoutingSched) Runtimes() []scheduler.AccountRuntime       { return nil }
func (f *fakeRoutingSched) CurrentRoutingPlan() *scheduler.RoutingPlan { return f.plan }

// fakeStoreNoRollup 只满足 Store 组合面（rollup 能力探测必失败）。
type fakeStoreNoRollup struct{ Store }

func (fakeStoreNoRollup) GetAllSettings(context.Context) ([]*domain.Setting, error) { return nil, nil }

func fpHex(fill byte) string {
	var b [32]byte
	for i := range b {
		b[i] = fill
	}
	return hex.EncodeToString(b[:])
}

var (
	fpA = fpHex(0xaa)
	fpB = fpHex(0xbb)
	fpC = fpHex(0xcc)
)

// routingFixturePlan 单路由计划（group 10 / openai-chat / "m"）：候选 1（fpA，
// 10000bp）、候选 2（fpB，15000bp）、候选 3（fpC，30000bp）。
func routingFixturePlan() (*scheduler.RoutingPlan, string, scheduler.RoutingPlanRoute) {
	rc, err := domain.RouteClassID(10, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	if err != nil {
		panic(err)
	}
	idHex := domain.RouteClassIDHex(rc)
	route := scheduler.RoutingPlanRoute{
		Ref: scheduler.RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m", OperationTag: string(domain.OpChatCompletions), RouteClassID: idHex},
		Candidates: []scheduler.RoutingPlanCandidate{
			{AccountID: 1, TemplateID: 11, LifecycleRevision: 3, UpstreamCostMultiplierBp: 10000, IdentityFingerprint: fpA, MappedModel: "m"},
			{AccountID: 2, TemplateID: 12, LifecycleRevision: 4, UpstreamCostMultiplierBp: 15000, IdentityFingerprint: fpB, MappedModel: "mapped-b"},
			{AccountID: 3, TemplateID: 13, LifecycleRevision: 1, UpstreamCostMultiplierBp: 30000, IdentityFingerprint: fpC, MappedModel: "m"},
		},
	}
	return &scheduler.RoutingPlan{Generation: 7, Routes: []scheduler.RoutingPlanRoute{route}}, idHex, route
}

func routingSvc(t *testing.T, fs *fakeStore, plan *scheduler.RoutingPlan) *Service {
	t.Helper()
	return New(fs, &fakeRoutingSched{plan: plan}, NopInvalidator{}, nil, nil, nil, nil)
}

func withRoutingLoss(t *testing.T, incomplete, overflow int64) {
	t.Helper()
	orig := routingLoss
	routingLoss = struct {
		incomplete func() int64
		overflow   func() int64
	}{func() int64 { return incomplete }, func() int64 { return overflow }}
	t.Cleanup(func() { routingLoss = orig })
}

var routingBase = time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

func mustFP(t *testing.T, hexStr string) domain.CandidateFingerprintVal {
	t.Helper()
	raw, err := domain.HexToID(hexStr)
	require.NoError(t, err)
	return domain.CandidateFingerprintVal(raw)
}

func ptrInt64(v int64) *int64 { return &v }

// --- plan explanation ---

func TestRoutingPlan_PassesThroughCurrentProjection(t *testing.T) {
	plan, _, _ := routingFixturePlan()
	svc := routingSvc(t, newFakeStore(), plan)
	got, err := svc.RoutingPlanExplanation()
	require.NoError(t, err)
	require.Same(t, plan, got)
	require.Equal(t, uint64(7), got.Generation)
}

func TestRoutingPlan_NotWired(t *testing.T) {
	svc := New(newFakeStore(), &fakeRuntimeProvider{}, NopInvalidator{}, nil, nil, nil, nil)
	_, err := svc.RoutingPlanExplanation()
	require.ErrorIs(t, err, errRoutingNotWired)
	svcNil := &Service{store: newFakeStore()}
	_, err = svcNil.RoutingPlanExplanation()
	require.ErrorIs(t, err, errRoutingNotWired)
}

// --- flow: validation ---

func TestRoutingFlow_WindowValidation(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	svc := routingSvc(t, newFakeStore(), plan)
	ctx := context.Background()

	_, err := svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: idHex, To: routingBase.Add(time.Hour)})
	require.ErrorIs(t, err, ErrInvalidInput, "zero from")
	_, err = svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase})
	require.ErrorIs(t, err, ErrInvalidInput, "to must be after from")
	_, err = svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(90*24*time.Hour + time.Nanosecond)})
	require.ErrorIs(t, err, ErrInvalidInput, "90d + 1ns exceeds the exact cap")
	_, err = svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(90 * 24 * time.Hour)})
	require.NoError(t, err, "exactly 90d is allowed")
}

func TestRoutingFlow_RouteCatalogValidation(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	svc := routingSvc(t, fs, plan)
	ctx := context.Background()

	_, err := svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: "nothex", From: routingBase, To: routingBase.Add(time.Hour)})
	require.ErrorIs(t, err, ErrInvalidInput, "non-hex route id")
	_, err = svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: fpHex(0x01), From: routingBase, To: routingBase.Add(time.Hour)})
	require.ErrorIs(t, err, ErrNotFound, "well-formed but not in current catalog")

	_, err = svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, int16(domain.RoutingIdentityVersion), fs.routingRollupCall.version, "rollup filter must pin current identity version")
	require.True(t, fs.routingRollupCall.from.Equal(routingBase))
	require.True(t, fs.routingRollupCall.to.Equal(routingBase.Add(time.Hour)))
}

func TestRoutingFlow_NotWired(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	svc := New(&fakeStoreNoRollup{}, &fakeRoutingSched{plan: plan}, NopInvalidator{}, nil, nil, nil, nil)
	_, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour)})
	require.ErrorIs(t, err, errRoutingNotWired)
}

// --- flow: conservation + loss separation ---

func TestRoutingFlow_ConservationAndLossSeparation(t *testing.T) {
	plan, idHex, route := routingFixturePlan()
	fs := newFakeStore()
	// 链 A：单跳 terminal（ordinal 1）；链 B：ordinal1 失败 → ordinal2 degraded
	// terminal；另含一条旧 generation（6）的完整链——行保留自身 generation，
	// 结果单独暴露当前计划 generation（stale/current 不混）。
	fs.routingFlowRows = []repository.RoutingFlowStat{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, Generation: 7, CandidateFingerprint: mustFP(t, route.Candidates[0].IdentityFingerprint), ChainCount: 5},
		{Ordinal: 1, Lane: "primary", AccountID: 2, Outcome: "5xx", IsTerminal: false, Generation: 7, CandidateFingerprint: mustFP(t, route.Candidates[1].IdentityFingerprint), ChainCount: 3},
		{Ordinal: 2, Lane: "degraded", AccountID: 2, PreviousAccountID: ptrInt64(2), PreviousOutcome: "5xx", TransitionReason: "failover", Outcome: "success", IsTerminal: true, Generation: 7, CandidateFingerprint: mustFP(t, route.Candidates[1].IdentityFingerprint), ChainCount: 3},
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, Generation: 6, CandidateFingerprint: mustFP(t, route.Candidates[0].IdentityFingerprint), ChainCount: 2},
	}
	withRoutingLoss(t, 11, 13)
	svc := routingSvc(t, fs, plan)

	res, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(24 * time.Hour)})
	require.NoError(t, err)
	require.Equal(t, idHex, res.RouteClassID)
	require.Equal(t, uint64(7), res.PlanGeneration)

	// 守恒：Attempt1（ordinal=1 链数和）= 保留 terminal 链数和。
	require.Equal(t, int64(10), res.FirstDispatchChains)
	require.Equal(t, res.FirstDispatchChains, res.TerminalChains, "complete-chain conservation")

	// lane 分组：(1,primary) 三行（gen7 两行 + gen6 一行，repo 序 stable），
	// (2,degraded) 一行。
	require.Len(t, res.Lanes, 2)
	require.Equal(t, int16(1), res.Lanes[0].Ordinal)
	require.Equal(t, "primary", res.Lanes[0].Lane)
	require.Len(t, res.Lanes[0].Edges, 3)
	require.Equal(t, int16(2), res.Lanes[1].Ordinal)
	require.Equal(t, "degraded", res.Lanes[1].Lane)
	require.Len(t, res.Lanes[1].Edges, 1)

	// retry 边携带前驱语义；terminal 边即该链 Final。
	retry := res.Lanes[1].Edges[0]
	require.NotNil(t, retry.PreviousAccountID)
	require.Equal(t, int64(2), *retry.PreviousAccountID)
	require.Equal(t, "5xx", retry.PreviousOutcome)
	require.Equal(t, "failover", retry.TransitionReason)
	require.True(t, retry.IsTerminal)
	require.Equal(t, int64(7), retry.Generation)

	// 旧 generation 行原样保留（不与 PlanGeneration 混淆）。
	require.Equal(t, int64(6), res.Lanes[0].Edges[2].Generation)

	// 丢失计数独立口径，与边/结局语义无关。
	require.Equal(t, int64(11), res.IncompleteChainDropped)
	require.Equal(t, int64(13), res.FlowOverflowDroppedChains)
	require.True(t, res.ProcessCrashLossUnobservable)
}

func TestRoutingFlow_FingerprintHexAndEmptyWindow(t *testing.T) {
	plan, idHex, route := routingFixturePlan()
	fs := newFakeStore()
	fs.routingFlowRows = []repository.RoutingFlowStat{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, Generation: 7, CandidateFingerprint: mustFP(t, route.Candidates[0].IdentityFingerprint), ChainCount: 1},
	}
	svc := routingSvc(t, fs, plan)
	res, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Minute)})
	require.NoError(t, err)
	require.Equal(t, route.Candidates[0].IdentityFingerprint, res.Lanes[0].Edges[0].CandidateFingerprint)

	fs.routingFlowRows = nil
	empty, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Minute)})
	require.NoError(t, err)
	require.Empty(t, empty.Lanes)
	require.Zero(t, empty.FirstDispatchChains)
	require.Zero(t, empty.TerminalChains)
}

// --- frontier ---

func TestRoutingFrontier_WindowAndLimitClamp(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	svc := routingSvc(t, fs, plan)
	ctx := context.Background()

	_, err := svc.QueryRoutingFrontier(ctx, RoutingFrontierQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(90*24*time.Hour + time.Second)})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.QueryRoutingFrontier(ctx, RoutingFrontierQuery{RouteID: fpHex(0x02), From: routingBase, To: routingBase.Add(time.Hour)})
	require.ErrorIs(t, err, ErrNotFound)

	// 250 行 → 缺省钳到 200；limit=2 → 2；limit=500 → 钳到 200。
	fs.routingQualityRows = make([]repository.RoutingQualityStat, 250)
	for i := range fs.routingQualityRows {
		fs.routingQualityRows[i].CandidateFingerprint = mustFP(t, fmt.Sprintf("%064x", i))
	}
	all, err := svc.QueryRoutingFrontier(ctx, RoutingFrontierQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, all.Candidates, 200, "default limit clamps to 200")
	two, err := svc.QueryRoutingFrontier(ctx, RoutingFrontierQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 2})
	require.NoError(t, err)
	require.Len(t, two.Candidates, 2)
	big, err := svc.QueryRoutingFrontier(ctx, RoutingFrontierQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 500})
	require.NoError(t, err)
	require.Len(t, big.Candidates, 200)
}

func TestRoutingFrontier_Semantics(t *testing.T) {
	plan, idHex, route := routingFixturePlan()
	require.Len(t, route.Candidates, 3)
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(1000), OutputPerM: int64Ptr(2000), Source: domain.PricingSourceLitellm},
	})
	require.NoError(t, err)
	svc := routingSvc(t, fs, plan)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))

	q32 := float64(int64(1) << 32)
	logged := math.Log(100)
	sumLog := int64(logged*q32) * 40
	sumSq := int64(logged*logged*q32) * 40
	fpUnknown := fpHex(0xdd)

	fs.routingQualityRows = []repository.RoutingQualityStat{
		// A：40/40，成本 3000（(1e6×1000+1e6×2000)/1e6 ×1.0）——高质量高成本。
		{CandidateFingerprint: mustFP(t, fpA), Attempts: 40, Successes: 40, TTFTN: 40, TTFTSumLogQ32: sumLog, TTFTSumSqLogQ32: sumSq, InputTokens: 40_000_000, OutputTokens: 40_000_000},
		// B：20/40，成本 1800（1200×1.5）——低成本侧。
		{CandidateFingerprint: mustFP(t, fpB), Attempts: 40, Successes: 20, TTFTN: 40, TTFTSumLogQ32: sumLog, TTFTSumSqLogQ32: sumSq, InputTokens: 20_000_000, OutputTokens: 2_000_000},
		// C：与 B 同 LCB、更高成本（1400×3.0=4200）→ 被 B 支配。
		{CandidateFingerprint: mustFP(t, fpC), Attempts: 40, Successes: 20, InputTokens: 20_000_000, OutputTokens: 4_000_000},
		// D：未知指纹（不在当前目录）→ 不参与支配。
		{CandidateFingerprint: mustFP(t, fpUnknown), Attempts: 40, Successes: 40, InputTokens: 40_000_000},
		// E：known 但零成功 → 成本不可知。
		{CandidateFingerprint: mustFP(t, fpA), Attempts: 40, Successes: 0},
	}

	res, err := svc.QueryRoutingFrontier(context.Background(), RoutingFrontierQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(24 * time.Hour)})
	require.NoError(t, err)
	require.Equal(t, uint64(7), res.PlanGeneration)
	require.Len(t, res.Candidates, 5)

	byFP := make(map[string]RoutingFrontierCandidate, 5)
	for _, c := range res.Candidates {
		byFP[fmt.Sprintf("%s:%d", c.CandidateFingerprint, c.Successes)] = c
	}

	a := byFP[fpA+":40"]
	require.True(t, a.Known)
	require.Equal(t, int64(1), a.AccountID)
	require.Equal(t, int64(11), a.TemplateID)
	require.Equal(t, int64(3), a.LifecycleRevision)
	require.Equal(t, "m", a.MappedModel)
	require.False(t, a.Insufficient)
	require.True(t, a.TTFTKnown)
	require.InDelta(t, 100.0, a.TTFTLCB, 1.0)
	require.InDelta(t, 100.0, a.TTFTUCB, 1.0)
	require.InDelta(t, scheduler.Wilson95(40, 40).LCB, a.SuccessLCB, 1e-12)
	require.True(t, a.CostKnown)
	require.Equal(t, int64(3000), a.CostPerSuccess)
	require.True(t, a.OnFrontier)

	b := byFP[fpB+":20"]
	require.True(t, b.Known)
	require.Equal(t, "mapped-b", b.MappedModel)
	require.True(t, b.CostKnown)
	require.Equal(t, int64(1800), b.CostPerSuccess)
	require.True(t, b.OnFrontier)

	c := byFP[fpC+":20"]
	require.True(t, c.Known)
	require.True(t, c.CostKnown)
	require.Equal(t, int64(4200), c.CostPerSuccess)
	require.False(t, c.OnFrontier, "same LCB as B at higher cost → dominated")
	require.InDelta(t, b.SuccessLCB, c.SuccessLCB, 1e-12)

	d := byFP[fpUnknown+":40"]
	require.False(t, d.Known)
	require.Zero(t, d.AccountID)
	require.False(t, d.CostKnown, "unknown candidates never get a computed cost")
	require.False(t, d.OnFrontier, "unknown candidates never dominate or get dominated")

	e := byFP[fpA+":0"]
	require.True(t, e.Known)
	require.False(t, e.CostKnown, "zero successes → cost unknown (compiler gate)")
	require.False(t, e.OnFrontier)
	require.False(t, e.Insufficient)

	// 确定性排序：前沿（LCB 降序）→ 非前沿（LCB 降序、成本升序、指纹升序）。
	require.Equal(t, []string{fpA, fpB, fpUnknown, fpC, fpA}, frontierFPs(res.Candidates))
}

func frontierFPs(cands []RoutingFrontierCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.CandidateFingerprint)
	}
	return out
}

func TestRoutingFrontier_InsufficientAndNoPrice(t *testing.T) {
	plan, idHex, route := routingFixturePlan()
	fs := newFakeStore() // 无价格表
	svc := routingSvc(t, fs, plan)
	fs.routingQualityRows = []repository.RoutingQualityStat{
		{CandidateFingerprint: mustFP(t, route.Candidates[0].IdentityFingerprint), Attempts: 10, Successes: 5, InputTokens: 5_000_000},
	}
	res, err := svc.QueryRoutingFrontier(context.Background(), RoutingFrontierQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, res.Candidates, 1)
	c := res.Candidates[0]
	require.True(t, c.Known)
	require.True(t, c.Insufficient, "attempts<30 shares the explore threshold")
	require.False(t, c.TTFTKnown, "n<30 has no TTFT interval")
	require.False(t, c.CostKnown, "no price entry → cost unknown")
	require.False(t, c.OnFrontier, "cost-unknown candidates never take the frontier")
}
