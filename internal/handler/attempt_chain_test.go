// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// admin API lane：GET /api/admin/ops/attempts/{request_id} 契约测试（spec
// 2026-11-18 §6 S3-9/10 的线格式一半）。走**真实 chi 路由**（路径参数绑定/日期
// 解析/400 语义都是生成物行为）；三源行由 fake 读面供给（真实写路径证据在
// internal/proxy 的 PG 用例里）。
//
// 断言以 snake_case 线格式为准：ordinal 必须是 JSON null（不是 0）、
// ordinal_unknown 必须是真布尔、sampling 未装配时必须**省略**字段（不是
// dropped_total: 0——那会让「没接计数」看起来像「零丢弃」）。
//
// 本文件还有两条「结构性守卫」：
//   - 采样接缝的零值：接缝是函数值，nil = 未装配（不存在 typed-nil 这种「非 nil
//     却指向 nil」的状态）。这里跑一次真实请求，断言 200 + sampling 省略；
//   - hop 全字段：13 个键逐个断言（含 commit_state/error_message 的 null 与
//     lane/outcome 的空串），置零或漏投影都会红。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// attemptChainFakeStore fake 读面：固定回放三源行（能力探测经方法集命中）。
type attemptChainFakeStore struct {
	*fakeStore
	rows  repository.AttemptChainRows
	gotID string
}

func (s *attemptChainFakeStore) QueryAttemptChain(_ context.Context, requestID string, _, _ time.Time) (repository.AttemptChainRows, error) {
	s.gotID = requestID
	return s.rows, nil
}

// attemptDropStub 丢弃计数桩（函数值接缝：桩就是返回固定值的闭包）。
func attemptDropStub(n int64) func() int64 { return func() int64 { return n } }

// attemptChainRouter 真实契约路由（BaseURL /api/admin）+ 注入的 ops 选项。
func attemptChainRouter(store *attemptChainFakeStore, opts OpsOptions) (*AdminAPI, http.Handler) {
	svc := service.New(service.Deps{Store: store, Invalidate: service.NopInvalidator{}, EmailCodeStore: store})
	h := New(svc, opts)
	return h, h.Router()
}

// attemptChainWindow 合法窗口（1 小时）。
func attemptChainWindow() string {
	from := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	return "?from=" + from.Format(time.RFC3339) + "&to=" + from.Add(time.Hour).Format(time.RFC3339)
}

// attemptChainWire 线格式视图（ordinal 用指针区分 null 与 0）。
type attemptChainWire struct {
	RequestID string `json:"request_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Truncated bool   `json:"truncated"`
	Hops      []struct {
		Source         string  `json:"source"`
		Ordinal        *int    `json:"ordinal"`
		OrdinalUnknown bool    `json:"ordinal_unknown"`
		AccountID      int64   `json:"account_id"`
		Lane           string  `json:"lane"`
		Format         string  `json:"format"`
		StatusCode     int     `json:"status_code"`
		Outcome        string  `json:"outcome"`
		ErrorType      string  `json:"error_type"`
		CommitState    *string `json:"commit_state"`
		ErrorMessage   *string `json:"error_message"`
		LatencyMS      int64   `json:"latency_ms"`
		CreatedAt      string  `json:"created_at"`
	} `json:"hops"`
	Sampling *struct {
		DroppedTotal int64 `json:"dropped_total"`
	} `json:"sampling"`
}

func decodeAttemptChain(t *testing.T, body []byte) (attemptChainWire, map[string]json.RawMessage) {
	t.Helper()
	var wire attemptChainWire
	require.NoError(t, json.Unmarshal(body, &wire), "body: %s", body)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	return wire, raw
}

// decodeHopKeys 每跳解码成「字段名 → 原始 JSON」，用来区分 null 与字段缺席。
func decodeHopKeys(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var raw struct {
		Hops []map[string]json.RawMessage `json:"hops"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	return raw.Hops
}

// attemptChainFixture 链：ordinal=1 的 attempt 行 + ordinal=1 的 err 行（去重后
// 消失）+ nil ordinal 的拒绝行（照常出现并标 ordinal_unknown）+ usage 终态。
func attemptChainFixture() repository.AttemptChainRows {
	one := int16(1)
	msg := "upstream 500"
	reject := "invalid key"
	commit := "upstream_responded"
	done := "response_started"
	created := time.Date(2026, 11, 18, 10, 0, 30, 0, time.UTC)
	return repository.AttemptChainRows{
		Attempt: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{{
			Ordinal: &one, AccountID: 11, Lane: "primary", Format: "openai-chat",
			StatusCode: 500, Outcome: "5xx", ErrorType: "5xx",
			CommitState: &commit, ErrorMessage: &msg, LatencyMS: 40, CreatedAt: created,
		}}},
		Err: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{
			{
				Ordinal: &one, AccountID: 11, Format: "openai-chat", StatusCode: 500,
				ErrorType: "5xx", ErrorMessage: &msg, LatencyMS: 41, CreatedAt: created.Add(time.Second),
			},
			{
				AccountID: 0, Format: "openai-chat", StatusCode: 401, ErrorType: "auth",
				ErrorMessage: &reject, LatencyMS: 1, CreatedAt: created.Add(-time.Minute),
			},
		}},
		Usage: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{{
			AccountID: 11, Format: "openai-chat", ErrorType: "none",
			CommitState: &done, LatencyMS: 900, CreatedAt: created.Add(2 * time.Second),
		}}},
	}
}

// TestGetAttemptChainWire hop **全字段**线格式：13 个键逐个断言，且区分
// 「null」（该源没有这一列/值未知）与「空串」（该源有列但没值）。
func TestGetAttemptChainWire(t *testing.T) {
	store := &attemptChainFakeStore{fakeStore: newFakeStore(), rows: attemptChainFixture()}
	_, router := attemptChainRouter(store, OpsOptions{AttemptSampling: attemptDropStub(7)})

	rec := doGET(t, router, "/api/admin/ops/attempts/req-1"+attemptChainWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.Bytes()
	wire, raw := decodeAttemptChain(t, body)
	hopKeys := decodeHopKeys(t, body)

	require.Equal(t, "req-1", wire.RequestID)
	require.Equal(t, "2026-11-18T10:00:00Z", wire.From)
	require.Equal(t, "2026-11-18T11:00:00Z", wire.To)
	require.False(t, wire.Truncated)
	require.Len(t, wire.Hops, 3, "同 ordinal 的 err 行必须被 attempt 行吸收；拒绝行与 usage 行各自保留")
	require.Len(t, hopKeys, 3)

	// 契约里 13 个 hop 键一个都不能少（缺投影 = 前端静默 blank）。
	for i, keys := range hopKeys {
		require.Len(t, keys, 13, "hop %d 必须输出 13 个键", i)
		for _, k := range []string{"source", "ordinal", "ordinal_unknown", "account_id", "lane",
			"format", "status_code", "outcome", "error_type", "commit_state", "error_message",
			"latency_ms", "created_at"} {
			require.Contains(t, keys, k, "hop %d missing %s", i, k)
		}
	}

	// ① nil ordinal 的拒绝行排在最前；「该源没有的列」是空串 / null，不是编造值。
	require.Equal(t, "err", wire.Hops[0].Source)
	require.Nil(t, wire.Hops[0].Ordinal)
	require.True(t, wire.Hops[0].OrdinalUnknown)
	require.Equal(t, int64(0), wire.Hops[0].AccountID)
	require.Equal(t, "", wire.Hops[0].Lane)
	require.Equal(t, "openai-chat", wire.Hops[0].Format)
	require.Equal(t, 401, wire.Hops[0].StatusCode)
	require.Equal(t, "", wire.Hops[0].Outcome)
	require.Equal(t, "auth", wire.Hops[0].ErrorType)
	require.Nil(t, wire.Hops[0].CommitState)
	require.NotNil(t, wire.Hops[0].ErrorMessage)
	require.Equal(t, "invalid key", *wire.Hops[0].ErrorMessage)
	require.Equal(t, int64(1), wire.Hops[0].LatencyMS)
	require.Equal(t, "2026-11-18T09:59:30Z", wire.Hops[0].CreatedAt)
	// null 与「缺字段」在线格式上是两件事：这几列必须是显式 null/空串。
	require.JSONEq(t, `null`, string(hopKeys[0]["ordinal"]))
	require.JSONEq(t, `null`, string(hopKeys[0]["commit_state"]))
	require.JSONEq(t, `""`, string(hopKeys[0]["lane"]))
	require.JSONEq(t, `""`, string(hopKeys[0]["outcome"]))

	// ② 去重后只剩 attempt 行（source=attempt，携带 attempt 腿的 outcome/lane）。
	require.Equal(t, "attempt", wire.Hops[1].Source)
	require.NotNil(t, wire.Hops[1].Ordinal)
	require.Equal(t, 1, *wire.Hops[1].Ordinal)
	require.False(t, wire.Hops[1].OrdinalUnknown)
	require.Equal(t, int64(11), wire.Hops[1].AccountID)
	require.Equal(t, "primary", wire.Hops[1].Lane)
	require.Equal(t, "openai-chat", wire.Hops[1].Format)
	require.Equal(t, 500, wire.Hops[1].StatusCode)
	require.Equal(t, "5xx", wire.Hops[1].Outcome)
	require.Equal(t, "5xx", wire.Hops[1].ErrorType)
	require.NotNil(t, wire.Hops[1].CommitState)
	require.Equal(t, "upstream_responded", *wire.Hops[1].CommitState)
	require.NotNil(t, wire.Hops[1].ErrorMessage)
	require.Equal(t, "upstream 500", *wire.Hops[1].ErrorMessage)
	require.Equal(t, int64(40), wire.Hops[1].LatencyMS)
	require.Equal(t, "2026-11-18T10:00:30Z", wire.Hops[1].CreatedAt)

	// ③ usage 终态恒在最后：ordinal null + ordinal_unknown=false（按设计没有序号，
	// 不是「不知道」）；error_message 是 null（usage_logs 没有该列）。
	require.Equal(t, "usage", wire.Hops[2].Source)
	require.Nil(t, wire.Hops[2].Ordinal)
	require.False(t, wire.Hops[2].OrdinalUnknown)
	require.Equal(t, int64(11), wire.Hops[2].AccountID)
	require.Equal(t, "", wire.Hops[2].Lane)
	require.Equal(t, 0, wire.Hops[2].StatusCode)
	require.Equal(t, "", wire.Hops[2].Outcome)
	require.Equal(t, "none", wire.Hops[2].ErrorType)
	require.NotNil(t, wire.Hops[2].CommitState)
	require.Equal(t, "response_started", *wire.Hops[2].CommitState)
	require.Nil(t, wire.Hops[2].ErrorMessage)
	require.Equal(t, int64(900), wire.Hops[2].LatencyMS)
	require.Equal(t, "2026-11-18T10:00:32Z", wire.Hops[2].CreatedAt)
	require.JSONEq(t, `null`, string(hopKeys[2]["ordinal"]))
	require.JSONEq(t, `null`, string(hopKeys[2]["error_message"]))

	require.NotNil(t, wire.Sampling)
	require.EqualValues(t, 7, wire.Sampling.DroppedTotal)
	require.Contains(t, raw, "sampling")
}

// TestGetAttemptChainNilSamplingFunc 采样接缝的零值（nil 函数值）= 未装配 ⇒
// 请求照常 200、sampling 字段缺席（不是 panic，也不是 dropped_total: 0）。
// 接缝是函数值而非接口：判空即判「有没有人装配」，typed-nil 在类型层面不可表达。
func TestGetAttemptChainNilSamplingFunc(t *testing.T) {
	var unwired func() int64 // 零值：不指向任何方法值
	store := &attemptChainFakeStore{fakeStore: newFakeStore(), rows: attemptChainFixture()}
	_, router := attemptChainRouter(store, OpsOptions{AttemptSampling: unwired})

	rec := doGET(t, router, "/api/admin/ops/attempts/req-1"+attemptChainWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, raw := decodeAttemptChain(t, rec.Body.Bytes())
	require.NotContains(t, raw, "sampling", "nil 函数值 ⇒ 未装配 ⇒ 字段缺席（不是 panic，也不是 0）")
}

// TestGetAttemptChainOmitsSamplingWhenUnwired 未装配计数面 ⇒ 省略 sampling 字段
// （不伪造 dropped_total: 0）。
func TestGetAttemptChainOmitsSamplingWhenUnwired(t *testing.T) {
	store := &attemptChainFakeStore{fakeStore: newFakeStore(), rows: attemptChainFixture()}
	_, router := attemptChainRouter(store, OpsOptions{})

	rec := doGET(t, router, "/api/admin/ops/attempts/req-1"+attemptChainWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, raw := decodeAttemptChain(t, rec.Body.Bytes())
	require.NotContains(t, raw, "sampling", "未装配 ⇒ 字段缺席")
}

// TestGetAttemptChainEmptyChainIs200 窗口外 request_id ⇒ 空链 + sampling，**不是 404**
// （spec §6 S3-10）。空 hops 必须是 [] 而不是 null。
func TestGetAttemptChainEmptyChainIs200(t *testing.T) {
	store := &attemptChainFakeStore{fakeStore: newFakeStore()}
	_, router := attemptChainRouter(store, OpsOptions{AttemptSampling: attemptDropStub(0)})

	rec := doGET(t, router, "/api/admin/ops/attempts/outside-window"+attemptChainWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	wire, raw := decodeAttemptChain(t, rec.Body.Bytes())
	require.Empty(t, wire.Hops)
	require.JSONEq(t, `[]`, string(raw["hops"]), "空链必须是 []（null 会让前端多一条空值分支）")
	require.NotNil(t, wire.Sampling)
	require.Zero(t, wire.Sampling.DroppedTotal)
}

// TestGetAttemptChainRequestIDDecoded 路径参数按 URL 解码后原样进读面（第 K 轮
// 的 request_id 形如 "<base>#rK"，前端必须 encodeURIComponent）。
func TestGetAttemptChainRequestIDDecoded(t *testing.T) {
	store := &attemptChainFakeStore{fakeStore: newFakeStore()}
	_, router := attemptChainRouter(store, OpsOptions{})

	rec := doGET(t, router, "/api/admin/ops/attempts/req%23r2"+attemptChainWindow())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "req#r2", store.gotID)
}

// TestGetAttemptChainRejectsBadWindow 窗口边界一律 400（不静默裁剪）：跨度超 7 天、
// from ≥ to、非法日期。
func TestGetAttemptChainRejectsBadWindow(t *testing.T) {
	for name, query := range map[string]string{
		"window too wide": "?from=2026-11-01T00:00:00Z&to=2026-11-18T00:00:00Z",
		"empty window":    "?from=2026-11-18T00:00:00Z&to=2026-11-18T00:00:00Z",
		"invalid date":    "?from=not-a-date&to=2026-11-18T00:00:00Z",
	} {
		t.Run(name, func(t *testing.T) {
			store := &attemptChainFakeStore{fakeStore: newFakeStore()}
			_, router := attemptChainRouter(store, OpsOptions{})
			rec := doGET(t, router, "/api/admin/ops/attempts/req-1"+query)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Empty(t, store.gotID, "400 不得打到读面")
		})
	}
}

// TestGetAttemptChainRejectsLongRequestID 路径 request_id 超过 128 字符 ⇒ 400
// （把整段文本塞进路径的滥用形态）。
func TestGetAttemptChainRejectsLongRequestID(t *testing.T) {
	store := &attemptChainFakeStore{fakeStore: newFakeStore()}
	_, router := attemptChainRouter(store, OpsOptions{})
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	rec := doGET(t, router, "/api/admin/ops/attempts/"+string(long)+attemptChainWindow())
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Empty(t, store.gotID)
}

// TestLogsResponsesCarryRequestID 「request_id 必填」不只是类型：两条日志响应在
// 运行时都必须带非空 RequestID（它是请求链的唯一入口；空值等于入口消失）。
func TestLogsResponsesCarryRequestID(t *testing.T) {
	store := newFakeStore()
	store.logs = []*domain.UsageLog{
		{ID: 1, RequestID: "req-usage", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone,
			CreatedAt: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)},
	}
	svc := service.New(service.Deps{Store: store, Invalidate: service.NopInvalidator{}, EmailCodeStore: store})
	router := New(svc).Router()
	window := "?from=2026-11-18T00:00:00Z&to=2026-11-18T23:59:59Z"

	usageRec := doGET(t, router, "/api/admin/usage_logs"+window)
	require.Equal(t, http.StatusOK, usageRec.Code, usageRec.Body.String())
	var usage LogsResponse
	require.NoError(t, json.Unmarshal(usageRec.Body.Bytes(), &usage))
	require.Len(t, usage.Rows, 1)
	require.NotEmpty(t, usage.Rows[0].RequestID, "usage_logs 条目必须带非空 request_id")

	errRec := doGET(t, router, "/api/admin/err_logs"+window)
	require.Equal(t, http.StatusOK, errRec.Code, errRec.Body.String())
	var errLogs ErrLogsResponse
	require.NoError(t, json.Unmarshal(errRec.Body.Bytes(), &errLogs))
	require.Len(t, errLogs.Rows, 1)
	require.NotEmpty(t, errLogs.Rows[0].RequestID, "err_logs 条目必须带非空 request_id")
}
