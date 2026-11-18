// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// admin API lane：/routing/retry 端点契约测试。走真实 chi 路由（参数绑定/日期
// 解析/400 语义都是生成物行为）；聚合行由 routingStore fake 供给（不触 PG）。
//
// 断言以 snake_case 线格式为准（解码 map 校验键集合与 null 语义）：四个比值
// 的分母 0 必须是 JSON null——不是 0（fixture 3 次全失败时 retry_success_rate
// 必须是 0，空窗口时必须是 null，两者不可互换）。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func (s *routingStore) QueryFlowFactStatsMulti(context.Context, []domain.RouteClassIDVal, time.Time, time.Time) ([]repository.RoutingFlowStat, error) {
	return s.flowRows, nil
}

// retryWire 解码用的线格式视图（null 用指针区分）。
type retryWire struct {
	Routes           int              `json:"routes"`
	QueriedAt        string           `json:"queried_at"`
	RequestCount     int64            `json:"request_count"`
	AttemptTotal     int64            `json:"attempt_total"`
	RetryRate        *float64         `json:"retry_rate"`
	AvgAttempts      *float64         `json:"avg_attempts"`
	ExhaustedRate    *float64         `json:"exhausted_rate"`
	RetrySuccessRate *float64         `json:"retry_success_rate"`
	ByReason         []map[string]any `json:"by_reason"`
}

func decodeRetry(t *testing.T, body []byte) (retryWire, map[string]json.RawMessage) {
	t.Helper()
	var wire retryWire
	require.NoError(t, json.Unmarshal(body, &wire), "body: %s", body)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	return wire, raw
}

// retryThreeFailures 1 请求 3 次全失败（spec §6 S1 的关键夹具）；只有末条边
// terminal（settle 在 fold 前强制标尾）。
func retryThreeFailures() []repository.RoutingFlowStat {
	return []repository.RoutingFlowStat{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "429", ChainCount: 1},
		{Ordinal: 2, Lane: "primary", AccountID: 2, Outcome: "5xx", ChainCount: 1},
		{Ordinal: 3, Lane: "primary", AccountID: 3, Outcome: "network", ChainCount: 1, IsTerminal: true},
	}
}

func TestGetRoutingRetryWithRoute(t *testing.T) {
	store := &routingStore{fakeStore: newFakeStore(), flowRows: retryThreeFailures()}
	plan, _ := routingFixturePlan(t)
	h := routingRouter(store, plan)

	// 历史 route（不在当前发布计划目录内）也必须查得到——route 只做 hex 解析。
	historical := routingFPHex(t, 0x7e)
	rec := doGET(t, h, "/api/admin/routing/retry?route="+historical+"&"+routingWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	wire, raw := decodeRetry(t, rec.Body.Bytes())
	require.Equal(t, 1, wire.Routes)
	require.NotEmpty(t, wire.QueriedAt)
	require.Equal(t, int64(1), wire.RequestCount)
	require.Equal(t, int64(3), wire.AttemptTotal)
	require.Equal(t, 1.0, *wire.RetryRate)
	// 上一版算式（Σ k·C_k / C₁）在这里会得 6.0。
	require.Equal(t, 3.0, *wire.AvgAttempts)
	require.Equal(t, 1.0, *wire.ExhaustedRate)
	// 重试过但重试后全失败 ⇒ 0，不是 null。
	require.NotNil(t, wire.RetrySuccessRate, "retry_success_rate must be 0, not null (R=1)")
	require.Equal(t, 0.0, *wire.RetrySuccessRate)
	require.Len(t, wire.ByReason, 3)
	// 线格式：by_reason 是对象数组（outcome + chain_count），不是自由 schema。
	for _, r := range wire.ByReason {
		require.Contains(t, r, "outcome")
		require.Contains(t, r, "chain_count")
		require.Len(t, r, 2)
	}
	// 空字符串 "null" 与 0 必须可区分：raw 里 retry_success_rate 是 0 不是 null。
	require.JSONEq(t, "0", string(raw["retry_success_rate"]))
}

func TestGetRoutingRetryRouteLess(t *testing.T) {
	rc1, err := domain.RouteClassID(10, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	rc2, err := domain.RouteClassID(11, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	store := &routingStore{
		fakeStore:    newFakeStore(),
		observedRows: []domain.RouteClassIDVal{rc1, rc2},
		flowRows: []repository.RoutingFlowStat{
			{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "429", ChainCount: 1},
			{Ordinal: 2, Lane: "primary", AccountID: 2, Outcome: "client_cancel", ChainCount: 1, IsTerminal: true},
		},
	}
	plan, _ := routingFixturePlan(t)
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/retry?"+routingWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	wire, _ := decodeRetry(t, rec.Body.Bytes())
	require.Equal(t, 2, wire.Routes, "routes = 参与计算的 route 数")
	require.Equal(t, int64(1), wire.RequestCount)
	// 第 2 次是客户端取消：取消不是耗尽。
	require.NotNil(t, wire.ExhaustedRate)
	require.Equal(t, 0.0, *wire.ExhaustedRate)
	require.NotNil(t, wire.RetrySuccessRate)
	require.Equal(t, 0.0, *wire.RetrySuccessRate)
	require.Len(t, wire.ByReason, 2, "client_cancel 单列，不并入失败边")
}

// TestGetRoutingRetryVoidOrdinal 是 v6 的端到端回归护栏：链 1(error) → 3(success)
// 没有 ordinal=2 边，旧的 C₂ 口径会在线上返回 retry_rate = 0 / retry_success_rate =
// null；正确口径 R = C₁ − T₁ = 1 ⇒ retry_rate 是 JSON 数字 1，不是 0 也不是 null。
func TestGetRoutingRetryVoidOrdinal(t *testing.T) {
	store := &routingStore{fakeStore: newFakeStore(), flowRows: []repository.RoutingFlowStat{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "error", ChainCount: 1},
		{Ordinal: 3, Lane: "primary", AccountID: 2, Outcome: "success", ChainCount: 1, IsTerminal: true},
	}}
	plan, _ := routingFixturePlan(t)
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/retry?route="+routingFPHex(t, 0x7e)+"&"+routingWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	wire, raw := decodeRetry(t, rec.Body.Bytes())
	require.Equal(t, int64(1), wire.RequestCount, "C₁")
	require.Equal(t, int64(2), wire.AttemptTotal, "两次尝试各一条边（ordinal 空洞不影响尝试数）")
	require.NotNil(t, wire.RetryRate, "空洞链重试过 ⇒ retry_rate 不是 null")
	require.Equal(t, 1.0, *wire.RetryRate)
	require.JSONEq(t, "1", string(raw["retry_rate"]), "线格式必须是 1，不是 0 / null")
	require.Equal(t, 2.0, *wire.AvgAttempts, "Σ C_k / C₁ = 2")
	require.Equal(t, 0.0, *wire.ExhaustedRate, "第 3 次成功 ⇒ 无耗尽")
	require.NotNil(t, wire.RetrySuccessRate, "重试后成功 ⇒ 1，不是 null")
	require.Equal(t, 1.0, *wire.RetrySuccessRate)
	require.Len(t, wire.ByReason, 1)
	require.Equal(t, "error", wire.ByReason[0]["outcome"])
}

func TestGetRoutingRetryTooManyRoutes(t *testing.T) {
	observed := make([]domain.RouteClassIDVal, 201)
	for i := range observed {
		observed[i][0] = byte(i)
		observed[i][1] = byte(i >> 8)
	}
	store := &routingStore{fakeStore: newFakeStore(), observedRows: observed}
	plan, _ := routingFixturePlan(t)
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/retry?"+routingWindow())
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body["error"], "pass route", "超限必须明确要求指定 route")
	require.NotContains(t, body["error"], "internal error")
}

func TestGetRoutingRetryEmptyWindow(t *testing.T) {
	store := &routingStore{fakeStore: newFakeStore()}
	plan, _ := routingFixturePlan(t)
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/retry?"+routingWindow())
	require.Equal(t, http.StatusOK, rec.Code, "空窗口恒 200，不是 404（body: %s）", rec.Body.String())
	wire, raw := decodeRetry(t, rec.Body.Bytes())
	require.Zero(t, wire.Routes)
	require.Zero(t, wire.RequestCount)
	require.Zero(t, wire.AttemptTotal)
	require.Nil(t, wire.RetryRate)
	require.Nil(t, wire.AvgAttempts)
	require.Nil(t, wire.ExhaustedRate)
	require.Nil(t, wire.RetrySuccessRate)
	require.NotNil(t, wire.ByReason)
	require.Empty(t, wire.ByReason)
	// 四个比值都必须是 null，且 by_reason 是 []（不是 null）。
	for _, k := range []string{"retry_rate", "avg_attempts", "exhausted_rate", "retry_success_rate"} {
		require.JSONEq(t, "null", string(raw[k]), "%s must serialize as null", k)
	}
	require.JSONEq(t, "[]", string(raw["by_reason"]))
}

func TestGetRoutingRetryInvalidRoute(t *testing.T) {
	store := &routingStore{fakeStore: newFakeStore()}
	plan, _ := routingFixturePlan(t)
	h := routingRouter(store, plan)

	rec := doGET(t, h, "/api/admin/routing/retry?route=nothex&"+routingWindow())
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// 少一个必填参数（窗口）也是 400——生成物语义。
	rec = doGET(t, h, "/api/admin/routing/retry")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
