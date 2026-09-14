// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// Todo 17 admin API lane：/routing/flow|frontier|plan 三端点的契约测试。
// 走真实 chi 路由（参数绑定/日期解析/400 语义都是生成物行为）；rollup 行由
// fakeStore 包装供给，计划目录由 fake provider 供给。断言以 snake_case 线格式
// 为准（解码 map 校验键集合精确），值语义校验复用生成类型。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
)

// --- fakes ---

// routingStore 在 fakeStore 之上补 rollup 聚合读能力（service 侧能力探测）。
type routingStore struct {
	*fakeStore
	flowRows    []repository.RoutingFlowStat
	qualityRows []repository.RoutingQualityStat
}

func (s *routingStore) QueryQualityRollupStats(context.Context, domain.RouteClassIDVal, int16, time.Time, time.Time) ([]repository.RoutingQualityStat, error) {
	return s.qualityRows, nil
}

func (s *routingStore) QueryFlowRollupStats(context.Context, domain.RouteClassIDVal, int16, time.Time, time.Time) ([]repository.RoutingFlowStat, error) {
	return s.flowRows, nil
}

var _ service.RoutingRollupReader = (*routingStore)(nil)

// routingSched 提供当前计划投影（RuntimeProvider 面沿用 fakeSched）。
type routingSched struct {
	fakeSched
	plan *scheduler.RoutingPlan
}

func (r routingSched) CurrentRoutingPlan() *scheduler.RoutingPlan { return r.plan }

func routingRouter(store *routingStore, plan *scheduler.RoutingPlan) http.Handler {
	svc := service.New(store, routingSched{plan: plan}, service.NopInvalidator{}, nil, nil, nil, nil)
	r := chi.NewRouter()
	r.Mount("/", New(svc).Router())
	return r
}

func doGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- fixtures（与 service lane routing_test 同构） ---

var routingBase = time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

func routingFPHex(t *testing.T, fill byte) string {
	t.Helper()
	var b [32]byte
	for i := range b {
		b[i] = fill
	}
	return hex.EncodeToString(b[:])
}

func routingMustFP(t *testing.T, hexStr string) domain.CandidateFingerprintVal {
	t.Helper()
	raw, err := domain.HexToID(hexStr)
	require.NoError(t, err)
	return domain.CandidateFingerprintVal(raw)
}

func routingWindow() string {
	return "from=" + routingBase.Format(time.RFC3339) + "&to=" + routingBase.Add(time.Hour).Format(time.RFC3339)
}

// routingFixturePlan 单路由计划（group 10 / openai-chat / "m"，generation 7），
// 候选 1/2/3 指纹 fpA/fpB/fpC。返回计划与路由类 hex ID。
func routingFixturePlan(t *testing.T) (*scheduler.RoutingPlan, string) {
	t.Helper()
	rc, err := domain.RouteClassID(10, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	idHex := domain.RouteClassIDHex(rc)
	plan := &scheduler.RoutingPlan{Generation: 7, Routes: []scheduler.RoutingPlanRoute{{
		Ref: scheduler.RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m", OperationTag: string(domain.OpChatCompletions), RouteClassID: idHex},
		Candidates: []scheduler.RoutingPlanCandidate{
			{AccountID: 1, TemplateID: 11, LifecycleRevision: 3, IdentityFingerprint: routingFPHex(t, 0xaa), MappedModel: "m"},
			{AccountID: 2, TemplateID: 12, LifecycleRevision: 4, IdentityFingerprint: routingFPHex(t, 0xbb), MappedModel: "mapped-b"},
		},
	}}}
	return plan, idHex
}

func i64p(v int64) *int64 { return &v }

// --- /routing/flow ---

func Test_RoutingFlow_ValidMappingAndLossFields(t *testing.T) {
	plan, idHex := routingFixturePlan(t)
	store := &routingStore{fakeStore: newFakeStore()}
	fpA, fpB := routingFPHex(t, 0xaa), routingFPHex(t, 0xbb)
	store.flowRows = []repository.RoutingFlowStat{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, Generation: 7, CandidateFingerprint: routingMustFP(t, fpA), ChainCount: 5},
		{Ordinal: 1, Lane: "primary", AccountID: 2, Outcome: "5xx", Generation: 7, CandidateFingerprint: routingMustFP(t, fpB), ChainCount: 3},
		{Ordinal: 2, Lane: "degraded", AccountID: 2, PreviousAccountID: i64p(2), PreviousOutcome: "5xx", TransitionReason: "failover", Outcome: "success", IsTerminal: true, Generation: 7, CandidateFingerprint: routingMustFP(t, fpB), ChainCount: 3},
	}
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/flow?route="+idHex+"&"+routingWindow())
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	// 丢失三口径精确字段（本进程 quality 计数器未被本包触碰 → 观测值为 0）。
	require.Contains(t, raw, "incomplete_chain_dropped")
	require.Contains(t, raw, "flow_overflow_dropped_chains")
	require.Equal(t, float64(0), raw["incomplete_chain_dropped"])
	require.Equal(t, float64(0), raw["flow_overflow_dropped_chains"])
	require.Equal(t, true, raw["process_crash_loss_unobservable"])
	require.Equal(t, float64(7), raw["plan_generation"])

	var res RoutingFlowResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, idHex, res.RouteClassId)
	require.Equal(t, int64(8), res.FirstDispatchChains)
	require.Equal(t, res.FirstDispatchChains, res.TerminalChains, "complete-chain conservation over the wire")
	require.Len(t, res.Lanes, 2)
	require.Equal(t, 1, res.Lanes[0].Ordinal)
	require.Equal(t, "primary", res.Lanes[0].Lane)
	require.Len(t, res.Lanes[0].Edges, 2)
	require.Nil(t, res.Lanes[0].Edges[0].PreviousAccountId, "首发边 previous_account_id = null")
	retry := res.Lanes[1].Edges[0]
	require.Equal(t, 2, res.Lanes[1].Ordinal)
	require.Equal(t, "degraded", res.Lanes[1].Lane)
	require.NotNil(t, retry.PreviousAccountId)
	require.Equal(t, int64(2), *retry.PreviousAccountId)
	require.Equal(t, "5xx", retry.PreviousOutcome)
	require.Equal(t, "failover", retry.TransitionReason)
	require.True(t, retry.IsTerminal)
	require.Equal(t, fpB, retry.CandidateFingerprint)
	require.Equal(t, int64(3), retry.ChainCount)
}

func Test_RoutingFlow_EmptyWindowKeepsEmptyArray(t *testing.T) {
	plan, idHex := routingFixturePlan(t)
	h := routingRouter(&routingStore{fakeStore: newFakeStore()}, plan)
	rec := doGET(t, h, "/api/admin/routing/flow?route="+idHex+"&"+routingWindow())
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"lanes":[]`, "空 lanes 必须序列化为 [] 而非 null")
}

func Test_RoutingFlow_Validation(t *testing.T) {
	plan, idHex := routingFixturePlan(t)
	h := routingRouter(&routingStore{fakeStore: newFakeStore()}, plan)

	rec := doGET(t, h, "/api/admin/routing/flow?route="+idHex+"&from="+routingBase.Format(time.RFC3339)+"&to="+routingBase.Add(91*24*time.Hour).Format(time.RFC3339))
	require.Equal(t, 400, rec.Code, ">90d window: %s", rec.Body.String())

	rec = doGET(t, h, "/api/admin/routing/flow?route=nothex&"+routingWindow())
	require.Equal(t, 400, rec.Code, "non-hex route: %s", rec.Body.String())

	rec = doGET(t, h, "/api/admin/routing/flow?route="+idHex)
	require.Equal(t, 400, rec.Code, "missing required from/to must fail binding")

	rec = doGET(t, h, "/api/admin/routing/flow?route="+routingFPHex(t, 0x01)+"&"+routingWindow())
	require.Equal(t, 404, rec.Code, "well-formed but unknown route → 404: %s", rec.Body.String())
}

// --- /routing/frontier ---

func Test_RoutingFrontier_Mapping(t *testing.T) {
	plan, idHex := routingFixturePlan(t)
	store := &routingStore{fakeStore: newFakeStore()}
	fpA, fpUnknown := routingFPHex(t, 0xaa), routingFPHex(t, 0xdd)
	store.qualityRows = []repository.RoutingQualityStat{
		{CandidateFingerprint: routingMustFP(t, fpA), Attempts: 40, Successes: 20},
		{CandidateFingerprint: routingMustFP(t, fpUnknown), Attempts: 10, Successes: 10},
	}
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/frontier?route="+idHex+"&"+routingWindow())
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var res RoutingFrontierResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, idHex, res.RouteClassId)
	require.Equal(t, int64(7), res.PlanGeneration)
	require.Len(t, res.Candidates, 2)

	// 排序 = success_lcb 降序（10/10 的 Wilson 下界高于 20/40），按指纹取回语义断言。
	byFP := map[string]RoutingFrontierCandidate{}
	for _, c := range res.Candidates {
		byFP[c.CandidateFingerprint] = c
	}
	require.Equal(t, res.Candidates[0].CandidateFingerprint, fpUnknown)
	known, unknown := byFP[fpA], byFP[fpUnknown]
	require.Equal(t, fpA, known.CandidateFingerprint)
	require.True(t, known.Known)
	require.Equal(t, int64(1), known.AccountId)
	require.Equal(t, int64(11), known.TemplateId)
	require.Equal(t, int64(3), known.LifecycleRevision)
	require.Equal(t, "m", known.MappedModel)
	require.Equal(t, int64(40), known.Attempts)
	require.False(t, known.CostKnown, "无价格表 → cost_known=false")
	require.False(t, known.OnFrontier)
	require.Equal(t, fpUnknown, unknown.CandidateFingerprint)
	require.False(t, unknown.Known)
	require.True(t, unknown.Insufficient, "attempts<30 → insufficient")

	// 线格式键集合精确（snake_case，18 字段全在）。
	var raw struct {
		Candidates []map[string]any `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Candidates[0], 18)
	for _, key := range []string{"candidate_fingerprint", "known", "account_id", "template_id", "lifecycle_revision",
		"quality_class_id", "mapped_model", "attempts", "successes", "success_lcb", "success_ucb",
		"ttft_lcb", "ttft_ucb", "ttft_known", "cost_per_success", "cost_known", "insufficient", "on_frontier"} {
		require.Contains(t, raw.Candidates[0], key)
	}
}

func Test_RoutingFrontier_LimitClamp(t *testing.T) {
	plan, idHex := routingFixturePlan(t)
	store := &routingStore{fakeStore: newFakeStore()}
	store.qualityRows = make([]repository.RoutingQualityStat, 250)
	for i := range store.qualityRows {
		store.qualityRows[i].CandidateFingerprint = routingMustFP(t, fmt.Sprintf("%064x", i))
	}
	h := routingRouter(store, plan)
	base := "/api/admin/routing/frontier?route=" + idHex + "&" + routingWindow()

	count := func(t *testing.T, url string) int {
		t.Helper()
		rec := doGET(t, h, url)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var res RoutingFrontierResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
		return len(res.Candidates)
	}
	require.Equal(t, 200, count(t, base), "缺省 limit → 200")
	require.Equal(t, 200, count(t, base+"&limit=500"), "limit>200 钳到 200（200 非 400）")
	require.Equal(t, 2, count(t, base+"&limit=2"))
	require.Equal(t, 200, count(t, base+"&limit=0"), "limit≤0 → 缺省 200")

	rec := doGET(t, h, "/api/admin/routing/frontier?route="+routingFPHex(t, 0x02)+"&"+routingWindow())
	require.Equal(t, 404, rec.Code, "unknown route → 404: %s", rec.Body.String())
}

// --- /routing/plan ---

func Test_RoutingPlan_GenerationAndOrder(t *testing.T) {
	rcA, err := domain.RouteClassID(10, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	rcB, err := domain.RouteClassID(5, domain.FormatAnthropic, "z", domain.OpAnthropicMessages)
	require.NoError(t, err)
	plan := &scheduler.RoutingPlan{Generation: 42, Routes: []scheduler.RoutingPlanRoute{
		{
			Ref:      scheduler.RouteRef{GroupID: 10, Format: "openai-chat", Model: "m", OperationTag: "chat.completions", RouteClassID: domain.RouteClassIDHex(rcA)},
			Primary:  []int64{7, 3},
			Explore:  scheduler.ExploreIDs{IDs: []int64{2, 1}, Weights: map[int64]int{2: 3, 1: 7}, Cumulative: []uint64{3, 10}, Total: 10, Fallback: []int64{1}},
			Degraded: []int64{9},
			Candidates: []scheduler.RoutingPlanCandidate{
				{AccountID: 1, TemplateID: 11, LifecycleRevision: 2, UpstreamCostMultiplierBp: 15000, IdentityFingerprint: routingFPHex(t, 0xbb), MappedModel: "m", QualityClassID: "qc"},
			},
		},
		{
			Ref: scheduler.RouteRef{GroupID: 5, Format: "anthropic-messages", Model: "z", OperationTag: "messages", RouteClassID: domain.RouteClassIDHex(rcB)},
		},
	}}
	h := routingRouter(&routingStore{fakeStore: newFakeStore()}, plan)

	rec := doGET(t, h, "/api/admin/routing/plan")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var res RoutingPlanResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, int64(42), res.Generation)
	require.Len(t, res.Routes, 2)
	// 直通投影序（发布字节守卫同序），handler 不重排。
	require.Equal(t, domain.RouteClassIDHex(rcA), res.Routes[0].Ref.RouteClassId)
	require.Equal(t, domain.RouteClassIDHex(rcB), res.Routes[1].Ref.RouteClassId)
	require.Equal(t, int64(10), res.Routes[0].Ref.GroupId)
	require.Equal(t, "chat.completions", res.Routes[0].Ref.OperationTag)

	// 通道发布序原样保留。
	require.Equal(t, []int64{7, 3}, res.Routes[0].Primary)
	require.Equal(t, []int64{9}, res.Routes[0].Degraded)
	require.Equal(t, []int64{2, 1}, res.Routes[0].Explore.Ids)
	require.Equal(t, []int64{3, 10}, res.Routes[0].Explore.Cumulative)
	require.Equal(t, int64(10), res.Routes[0].Explore.Total)
	require.Equal(t, []int64{1}, res.Routes[0].Explore.Fallback)
	require.Equal(t, map[string]int{"1": 7, "2": 3}, res.Routes[0].Explore.Weights)

	require.Equal(t, int64(1), res.Routes[0].Candidates[0].AccountId)
	require.Equal(t, 15000, res.Routes[0].Candidates[0].UpstreamCostMultiplierBp)
	require.Equal(t, routingFPHex(t, 0xbb), res.Routes[0].Candidates[0].IdentityFingerprint)

	// 空 lane 序列化为 []（缺省零值路由）。
	require.Contains(t, rec.Body.String(), `"primary":[]`)
	require.Contains(t, rec.Body.String(), `"candidates":[]`)
}

func Test_RoutingPlan_EmptyViewIsNotError(t *testing.T) {
	h := routingRouter(&routingStore{fakeStore: newFakeStore()}, &scheduler.RoutingPlan{Routes: []scheduler.RoutingPlanRoute{}})
	rec := doGET(t, h, "/api/admin/routing/plan")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.True(t, strings.Contains(rec.Body.String(), `"generation":0`))
	require.Contains(t, rec.Body.String(), `"routes":[]`, "空计划 routes 必须是 [] 而非 null")
}

func Test_RoutingPlan_Incident(t *testing.T) {
	plan := &scheduler.RoutingPlan{Generation: 7, Routes: []scheduler.RoutingPlanRoute{
		{
			Ref:      scheduler.RouteRef{GroupID: 10, Format: "openai-chat", Model: "m", OperationTag: "chat.completions", RouteClassID: "rc-a"},
			Primary:  []int64{1},
			Incident: scheduler.RouteIncident{Active: true, Kind: "domain", Comparable: 3, Degraded: 2, Domains: 1, EvaluatedMinute: 1757745600},
		},
		{
			Ref:     scheduler.RouteRef{GroupID: 20, Format: "openai-chat", Model: "m", OperationTag: "chat.completions", RouteClassID: "rc-b"},
			Primary: []int64{4},
		},
	}}
	h := routingRouter(&routingStore{fakeStore: newFakeStore()}, plan)

	rec := doGET(t, h, "/api/admin/routing/plan")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var res RoutingPlanResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Len(t, res.Routes, 2)

	active := res.Routes[0].Incident
	require.True(t, active.Active)
	require.Equal(t, "domain", active.Kind)
	require.Equal(t, 3, active.Comparable)
	require.Equal(t, 2, active.Degraded)
	require.Equal(t, 1, active.Domains)
	require.Equal(t, int64(1757745600), active.EvaluatedMinute)

	quiet := res.Routes[1].Incident
	require.False(t, quiet.Active)
	require.Equal(t, "", quiet.Kind)
	require.Equal(t, 0, quiet.Comparable)
	require.Equal(t, 0, quiet.Degraded)
	require.Equal(t, 0, quiet.Domains)
	require.Equal(t, int64(0), quiet.EvaluatedMinute)
}
