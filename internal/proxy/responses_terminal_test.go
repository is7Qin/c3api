// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// spec-responses-terminal-truth：Responses 流终止态真理（HTTP SSE 面）。
//
// 客户端判据（pi-ai openai-responses-shared.js 的 processResponsesStream）读完整条
// 流后要求出现过 response.completed / response.failed / response.incomplete 之一，
// 否则报 "stream ended before a terminal response event"。它**不认** `event: error`、
// 也不认无 type 的裸错误载荷。本文件把该判据写成测试：每个用例都按 SSE 切帧取
// 「最后一个事件的 type」，必须落在终态集内。
package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// Responses 透传帧 fixture（真实上游形态：event 行 + data JSON）。
const (
	respTermCreated    = `{"type":"response.created","response":{"id":"resp_t","object":"response","status":"in_progress","model":"gpt-4o"}}`
	respTermDelta      = `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`
	respTermDone       = `{"type":"response.completed","response":{"id":"resp_t","object":"response","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"input_tokens_details":{"cached_tokens":1}}}}`
	respTermFailed     = `{"type":"response.failed","response":{"id":"resp_t","object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}}`
	respTermIncomplete = `{"type":"response.incomplete","response":{"id":"resp_t","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`
)

// frameTail 帧的收尾形态——relay 的三种真实产物（relay.go:404-434、:264-272）。
type frameTail int

const (
	tailComplete frameTail = iota // 完整帧：`data:` 行 + 结尾空行（正常切帧）
	tailLineOnly                  // 末行完整、只缺结尾空行（EOF 时已切出的最后一个整行）
	tailNoTerm                    // 无行终止符的残尾（rb.rest() 原样切出的半截末行）
)

// sseData 组一帧 SSE（event 名为空 = data-only 帧），含结尾空行——完整帧形态。
// 全文件**唯一**的 wire 拼装点：frameEvent 与集成用例共用，避免同一格式两份真相
// 漂移后单测测的是 relay 不会产出的形态。
func sseData(event, data string) string {
	if event == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// frameEvent 造一帧 relay 真实形态的事件：wire 由 sseData 给出（唯一真相），再按
// tail 削掉结尾。三种收尾都必须造得出来——终态判据看的是 Raw 的空行后缀
// （sserelay.Event.Raw 契约：完整原始帧，含结尾空行），只造完整帧测不到残帧路径。
func frameEvent(event, data string, tail frameTail) sserelay.Event {
	raw := sseData(event, data)
	switch tail {
	case tailLineOnly: // 去掉空行，保留 `data:` 行自己的行终止符
		raw = strings.TrimSuffix(raw, "\n")
	case tailNoTerm: // 连行终止符也不留
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\n")
	}
	return sserelay.Event{Raw: []byte(raw), Event: []byte(event), Data: []byte(data)}
}

// terminalPayload 造一个约 size 字节的 response.completed 载荷（type 在前、
// pad 补长），用于「终态帧可达 MB 级」的回归锁。
func terminalPayload(size int) string {
	const head = `{"type":"response.completed","response":{"id":"r","pad":"`
	const tail = `"}}`
	pad := size - len(head) - len(tail)
	if pad < 0 {
		pad = 0
	}
	return head + strings.Repeat("a", pad) + tail
}

// sseTopLevelTypes 按 SSE 空行切帧，返回每帧 data 载荷的顶层 type（多行 data 合并；
// 无 type / 非 JSON（含 [DONE]）记空串）。**测试侧独立实现**——不复用生产判定，
// 否则变异（删判定/恒真）打不红。
func sseTopLevelTypes(body string) []string {
	var out []string
	// 按 wire 宽容解析（R2 momus C3）：CRLF 先归一到 LF；`data:` 后的空格可选
	// （SSE 规范只要求冒号）。断言不得依赖本仓自己写出的那一种帧格式。
	for _, block := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		if block == "" {
			continue
		}
		var data []string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if payload, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(payload, " "))
			}
		}
		if len(data) == 0 {
			continue // 纯 event:/注释块不构成事件
		}
		typ := ""
		var m map[string]any
		if json.Unmarshal([]byte(strings.Join(data, "\n")), &m) == nil {
			typ, _ = m["type"].(string)
		}
		out = append(out, typ)
	}
	return out
}

// lastTypedEvent 最后一个带 type 的事件名（pi-ai 判据的观察点；空 = 没有）。
func lastTypedEvent(types []string) string {
	for i := len(types) - 1; i >= 0; i-- {
		if types[i] != "" {
			return types[i]
		}
	}
	return ""
}

// isPiTerminal 客户端判据：只有这三个事件算终态。
func isPiTerminal(typ string) bool {
	switch typ {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	}
	return false
}

// countType 统计某 type 出现次数。
func countType(types []string, typ string) int {
	n := 0
	for _, got := range types {
		if got == typ {
			n++
		}
	}
	return n
}

// logRows 捕获行的快照（调用方不再持锁）。
func logRows(s *captureLogStore) []*domain.UsageLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*domain.UsageLog(nil), s.logs...)
}

// uniqRows 按**行指针**去重：已交付的失败是双轨行（usage_logs 可计费 + err_logs
// 明细），同一个 *UsageLog 会被捕获 store 插入两次；不同尝试是不同指针，不会被
// 合并。
func uniqRows(rows []*domain.UsageLog) []*domain.UsageLog {
	seen := make(map[*domain.UsageLog]bool, len(rows))
	out := rows[:0]
	for _, l := range rows {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// fakeResponsesRawStream /v1/responses 上游：200 + 逐字节原样写出 raw 后**干净
// EOF**（不补 [DONE]、不补终态）——透传车道终止态判定的基座。status 非 200 时只
// 写状态码（204 = 无 body、无 SSE）。
func fakeResponsesRawStream(t *testing.T, status int, raw string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, raw)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runResponsesPassthrough 走真实 /v1/responses HTTP 面（api-key 账号 → 原始字节
// 透传车道），返回响应记录器与捕获日志；断言前把 usage / err_logs 两个异步通道
// 排空。
func runResponsesPassthrough(t *testing.T, upstream string) (*httptest.ResponseRecorder, []*domain.UsageLog) {
	t.Helper()
	store := &captureLogStore{}
	p := newTestProxyResponses(t, upstream, 1, store)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	return rec, uniqRows(logRows(store))
}

// TestResponsesPassthroughTruncationAppendsFailedTerminal spec §4-1：上游发
// created+delta 后干净 EOF ⇒ 补 error 帧 + response.failed 终态，且记失败
// （无 status ⇒ ErrNetwork）。
func TestResponsesPassthroughTruncationAppendsFailedTerminal(t *testing.T) {
	up := fakeResponsesRawStream(t, http.StatusOK,
		sseData("response.created", respTermCreated)+sseData("response.output_text.delta", respTermDelta))
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.Contains(t, body, "event: error\ndata: ", "已提交截断必须补 error 帧。body=%s", body)
	require.Contains(t, body, `"type":"response.failed"`, "Responses 必须补终态帧（pi-ai 判据）。body=%s", body)
	require.True(t, isPiTerminal(lastTypedEvent(types)), "最后一个事件的 type 必须是终态。events=%v", types)
	require.Equal(t, 1, countType(types, "response.failed"), "终态恰一条。events=%v", types)
	require.Contains(t, body, "without a terminal response event", "错误帧文案取哨兵原文（不退化）")

	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType, "截断 = network（序 5）")
	require.Equal(t, 0, rows[0].StatusCode, "上游没给过状态码 ⇒ 0（不合成）")
}

// TestResponsesPassthroughCompletedSingleTerminal spec §4-2：上游发终态 ⇒ 终态恰
// 一条、无 response.failed、按成功记账。
func TestResponsesPassthroughCompletedSingleTerminal(t *testing.T) {
	up := fakeResponsesRawStream(t, http.StatusOK,
		sseData("response.created", respTermCreated)+sseData("response.output_text.delta", respTermDelta)+sseData("response.completed", respTermDone))
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.Equal(t, "response.completed", lastTypedEvent(types), "events=%v", types)
	require.Zero(t, countType(types, "response.failed"), "已见终态不得再补 failed。body=%s", body)
	require.NotContains(t, body, "event: error")

	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType)
	require.Equal(t, http.StatusOK, rows[0].StatusCode)
}

// TestResponsesPassthroughTrailingGarbageKeepsSingleTerminal spec §4-3：终态之后
// 再发半截 JSON 行（干净 EOF）⇒ 终态仍恰一条、无 failed、成功。
func TestResponsesPassthroughTrailingGarbageKeepsSingleTerminal(t *testing.T) {
	raw := sseData("response.created", respTermCreated) +
		sseData("response.completed", respTermDone) +
		`data: {"type":"response.comple` // 无换行结尾的半截行（EOF 处被当空行派发）
	up := fakeResponsesRawStream(t, http.StatusOK, raw)
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.Equal(t, 1, countType(types, "response.completed"), "events=%v", types)
	require.Zero(t, countType(types, "response.failed"), "尾随垃圾不得触发第二终态。body=%s", body)
	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType)
}

// TestResponsesPassthroughTruncatedTerminalFrameIsNotTerminal spec §3.1：终态帧
// 本身被 EOF 截断（type 已完整、JSON 不合法）不得算「已见终态」——json.Valid
// 复核就是为它存在的。
func TestResponsesPassthroughTruncatedTerminalFrameIsNotTerminal(t *testing.T) {
	raw := sseData("response.created", respTermCreated) +
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_t"`
	up := fakeResponsesRawStream(t, http.StatusOK, raw)
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.True(t, isPiTerminal(lastTypedEvent(types)), "半截终态不算终态 ⇒ 必须补 failed。events=%v body=%s", types, body)
	require.Equal(t, 1, countType(types, "response.failed"))
	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
}

// TestResponsesPassthroughSubstringDeltaIsNotTerminal spec §4-5：delta 正文里含
// `"type":"response.completed"` ⇒ 仍不是终态（顶层判定）。
func TestResponsesPassthroughSubstringDeltaIsNotTerminal(t *testing.T) {
	delta := `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"{\"type\":\"response.completed\"}"}`
	up := fakeResponsesRawStream(t, http.StatusOK,
		sseData("response.created", respTermCreated)+sseData("response.output_text.delta", delta))
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.Equal(t, 1, countType(types, "response.failed"), "正文子串不得当终态。events=%v", types)
	require.True(t, isPiTerminal(lastTypedEvent(types)))
	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
}

// TestResponsesPassthroughIncompleteAndFailedAreTerminal spec §4-6：incomplete /
// failed 也算终态（不追加第二条）。
func TestResponsesPassthroughIncompleteAndFailedAreTerminal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		data  string
		want  int // 该终态事件在流里出现次数
	}{
		{"incomplete", "response.incomplete", respTermIncomplete, 1},
		{"failed", "response.failed", respTermFailed, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeResponsesRawStream(t, http.StatusOK,
				sseData("response.created", respTermCreated)+sseData(tc.event, tc.data))
			rec, rows := runResponsesPassthrough(t, up.URL)

			body := rec.Body.String()
			types := sseTopLevelTypes(body)
			require.Equal(t, tc.event, lastTypedEvent(types), "events=%v", types)
			require.Equal(t, tc.want, countType(types, tc.event), "终态恰一条，不追加。body=%s", body)
			terminals := 0
			for _, typ := range types {
				if isPiTerminal(typ) {
					terminals++
				}
			}
			require.Equal(t, 1, terminals, "一条流只允许一个终态事件。events=%v", types)
			require.Len(t, rows, 1)
			require.Equal(t, domain.ErrNone, rows[0].ErrorType)
		})
	}
}

// TestResponsesPassthroughErrorAfterTerminalStaysSuccess spec §3.3：上游发完终态
// 之后尾块读错（真读错，unexpected EOF）⇒ 客户端已拿到终态，按成功收口、不补
// 第二条终态、不翻失败记账。
func TestResponsesPassthroughErrorAfterTerminalStaysSuccess(t *testing.T) {
	// 声明比实际多 64 字节的 Content-Length：读完终态帧后必是读错。
	up, _ := newCodexAbortUpstream(t, respTermDone)
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.Equal(t, "response.completed", lastTypedEvent(types), "events=%v body=%s", types, body)
	require.Zero(t, countType(types, "response.failed"), "已见终态后读错不得追加第二终态。body=%s", body)
	require.NotContains(t, body, "event: error", "已见终态后读错不得补错误帧。body=%s", body)

	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType, "已见终态 ⇒ 成功（不把客户端已收到的流翻成失败）")
	require.Equal(t, http.StatusOK, rows[0].StatusCode)
}

// TestResponsesPassthroughClientCancelAfterTerminalStaysClientCancel R2 评审
// must-fix（黄金路径）：**已见终态后客户端取消**不得被「已见终态 ⇒ 成功收口」
// 吞成 success——它必须仍记 client_cancel/abort（并释放并发槽），同时客户端
// 不得收到第二条终态或 error 帧（交付已完成的事实不能变）。
//
// 上游：写完终态后**不关连接**（阻塞在测试控制的 channel 上），客户端在确认
// 收到终态后 cancel 请求 ctx。禁 time.Sleep：等待全部用 require.Eventually。
func TestResponsesPassthroughClientCancelAfterTerminalStaysClientCancel(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseData("response.created", respTermCreated)+sseData("response.completed", respTermDone))
		w.(http.Flusher).Flush()
		// 终态已下发，但连接不关：客户端在此之后取消。
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		up.Close()
	})

	store := &captureLogStore{}
	p := newTestProxyResponses(t, up.URL, 1, store)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponses))
	t.Cleanup(gw.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// 客户端侧累积已读字节（互斥：读 goroutine 与断言并发）。
	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
	require.Eventually(t, func() bool {
		return strings.Contains(snapshot(), `"type":"response.completed"`)
	}, 3*time.Second, 10*time.Millisecond, "客户端必须先拿到终态（body=%s）", snapshot())

	cancel() // 已见终态之后客户端取消
	<-readDone

	// 服务端收尾（finish→Release）发生在 handler 内：等行进入记录器队列再排空。
	require.Eventually(t, func() bool {
		store.mu.Lock()
		n := len(store.logs)
		store.mu.Unlock()
		return n > 0 || p.rec.Pending() > 0
	}, 3*time.Second, 10*time.Millisecond, "取消必须被记录（漏记即 suCCESS 吞并的回归）")
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))

	rows := uniqRows(logRows(store))
	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrAbort, rows[0].ErrorType,
		"已见终态后取消仍是客户端侧结束（不得被终态收口吞成 success）")
	require.Equal(t, http.StatusOK, rows[0].StatusCode, "abort 行 status 200（响应头早已提交）")
	require.NotNil(t, rows[0].CommitState)
	require.Equal(t, CommitResponseStarted.String(), *rows[0].CommitState, "已交付 + 客户端结束 ⇒ response_started")

	body := snapshot()
	types := sseTopLevelTypes(body)
	require.Equal(t, 1, countType(types, "response.completed"), "终态恰一条。body=%s", body)
	require.Zero(t, countType(types, "response.failed"), "取消不得追加第二条终态。body=%s", body)
	require.NotContains(t, body, "event: error", "取消不补错误帧。body=%s", body)

	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "abort 必须释放并发槽")
}

// TestResponsesPassthroughCommentOnlyAddsTerminal spec §4-9：只有注释帧后干净
// EOF ⇒ 补 failed，且走**已提交**分支（注释帧是业务字节 ⇒ sent_ambiguous）。
func TestResponsesPassthroughCommentOnlyAddsTerminal(t *testing.T) {
	up := fakeResponsesRawStream(t, http.StatusOK, ": ping\n\n")
	rec, rows := runResponsesPassthrough(t, up.URL)

	body := rec.Body.String()
	types := sseTopLevelTypes(body)
	require.Contains(t, body, `"type":"response.failed"`, "body=%s", body)
	require.True(t, isPiTerminal(lastTypedEvent(types)), "events=%v", types)
	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType, "零终态 ⇒ 截断失败")
	require.NotNil(t, rows[0].CommitState)
	require.Equal(t, CommitSentAmbiguous.String(), *rows[0].CommitState, "注释帧已下行 ⇒ 已提交分支")
}

// TestResponsesPassthroughUncommittedEmptyStreamFailsOver spec §4-8/§4-10：0 业务
// 帧 + 干净 EOF ⇒ 交 pipeline（非 200 空流）——换号后客户端只见第二条完整流。
func TestResponsesPassthroughUncommittedEmptyStreamFailsOver(t *testing.T) {
	// 第一次调用（首账号）返回 200 空 body，之后返回完整流：不依赖选号顺序。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if calls > 1 {
			_, _ = io.WriteString(w, sseData("response.created", respTermCreated)+sseData("response.completed", respTermDone))
		}
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: srv.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"},
	}
	newAcc := func(id int64) *domain.Account {
		return &domain.Account{
			ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-upstream",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
		}
	}
	store := &captureLogStore{}
	rec := usageRecorder(store)
	p := newTestProxyTplAccountsRec(t, map[int64][]*domain.Account{10: {newAcc(1), newAcc(2)}}, true, 30*time.Second, rec, nil, errLogStoreFrom(store))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleResponses(w, req)
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))

	require.Equal(t, http.StatusOK, w.Code, "换号后第二个账号成功。body=%s", w.Body.String())
	body := w.Body.String()
	types := sseTopLevelTypes(body)
	require.Equal(t, 1, countType(types, "response.completed"), "客户端只见第二条完整流。body=%s", body)
	require.Zero(t, countType(types, "response.failed"))
	require.Equal(t, 2, calls, "首个账号的空流不被当成成功（换号一次）")

	rows := uniqRows(logRows(store))
	require.NotEmpty(t, rows)
	success := 0
	for _, l := range rows {
		if l.ErrorType != domain.ErrNone {
			continue
		}
		success++
		require.Equal(t, http.StatusOK, l.StatusCode)
	}
	require.Equal(t, 1, success, "只有获胜尝试记成功（空流尝试不得记成功）")
}

// TestResponsesPassthroughEmpty200And204NotStream spec §4-10：200 空 body ⇒ 非 200
// 空流（交 pipeline）；204 ⇒ streamUpstreamStatus → 502，不进 relay。
func TestResponsesPassthroughEmpty200And204NotStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"200_empty", http.StatusOK},
		{"204", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeResponsesRawStream(t, tc.status, "")
			rec, _ := runResponsesPassthrough(t, up.URL)
			require.NotEqual(t, http.StatusOK, rec.Code, "空流不得以 200 收场。body=%s", rec.Body.String())
			require.NotContains(t, rec.Body.String(), "event: ", "未提交 ⇒ 不写 SSE 帧。body=%s", rec.Body.String())
		})
	}
}

// TestIsResponsesTerminal spec §3.1/§4-4/§4-5/§4-6：终态嗅探的两个来源 + 顶层
// 判定 + 帧完整性（Raw 的空行后缀）。事件一律按 relay 真实形态构造 Raw，否则
// 测的是「Raw 恒空」的假路径。
func TestIsResponsesTerminal(t *testing.T) {
	terminal := func(name, data string) bool { return isResponsesTerminal(frameEvent(name, data, tailComplete)) }
	truncated := func(name, data string) bool { return isResponsesTerminal(frameEvent(name, data, tailLineOnly)) }
	// 两个来源都认。
	require.True(t, terminal("response.completed", `{"response":{"id":"r"}}`), "只有 event: 名也算终态")
	require.True(t, terminal("", `{"type":"response.completed","response":{"id":"r"}}`), "只有 data.type 也算终态")
	require.True(t, terminal("response.failed", `{"type":"response.failed","response":{"status":"failed"}}`))
	require.True(t, terminal("response.incomplete", `{"type":"response.incomplete"}`))
	// 顶层判定：子串/嵌套同名键不算。
	require.False(t, terminal("response.output_text.delta", `{"type":"response.output_text.delta","delta":"{\"type\":\"response.completed\"}"}`),
		"正文子串不得当终态")
	require.False(t, terminal("", `{"delta":"response.completed"}`))
	require.False(t, terminal("response.created", `{"type":"response.created"}`))
	// 完整性复核：type 已完整但载荷被 EOF 截断（缺结尾空行）⇒ 不算终态。
	require.False(t, truncated("", `{"type":"response.completed","response":{"id":"r"`), "半截 JSON 不算终态")
	require.False(t, truncated("response.completed", `{"type":"response.completed"`), "event 名 + 半截 data 也不算")
	// 空 / 非 JSON / 结构未闭合（scanKeyValue 直接判定无该键）。
	require.False(t, terminal("", ""))
	require.False(t, terminal("", "not-json"))
	require.False(t, terminal("", `{"type":`))
	require.False(t, terminal("", `{"type":"`))
	// 判据边界：完整性只看 ev.Raw 的空行后缀，**不再看 data 的 JSON 合法性**——
	// 旧 json.Valid 复核的替代物就是它，两者不是「与」的关系。名字已命中 + 帧已
	// 完整收齐 ⇒ 终态，与载荷能否解码无关。
	//
	// 这是**故意的不一致 Event**（Raw 是完整帧、Data 是半截 JSON）：锚定的是判据
	// 边界而非 relay 的真实产物（生产路径 Raw 恒与 Data 同源）。没有这条，"把整帧
	// json 校验加回来"这种退化不会让任何用例变红——json.Valid 在该载荷形状下不
	// 分配，"1 MB 零分配"断言对它没有鉴别力。
	require.True(t, isResponsesTerminal(sserelay.Event{
		Raw:   []byte("event: response.completed\ndata: {\"type\":\"response.completed\"\n\n"),
		Event: []byte("response.completed"),
		Data:  []byte(`{"type":"response.completed"`), // 刻意不合法
	}), "判定不得依赖 data 的 JSON 合法性")
}

// TestIsResponsesTerminalFrameCompleteness spec §5.3 守卫：帧完整性判据必须同时
// 满足「两种行终止符都认」与「必须两个相邻终止符」。用例逐条 t.Run——同一子测试内
// require 会 fail-fast，判据退化后就看不到后面的守卫。
//
// 变异自证：退化成只认 bytes.HasSuffix(raw, "\n\n") ⇒ 纯 CRLF 与「LF 行 + CRLF 空行」
// 两条完整帧用例红；退化成「只认单个行终止符」⇒ 两条缺空行残帧用例红。
func TestIsResponsesTerminalFrameCompleteness(t *testing.T) {
	const data = `{"type":"response.completed"}`
	// frame 用两条行终止符拼一帧：prev 收 data 行、blank 作空行（"" = 缺空行）。
	// 这里的 raw 必须逐字节手拼——被测的就是行终止符组合本身。
	frame := func(prev, blank string) sserelay.Event {
		return sserelay.Event{
			Raw:   []byte("event: response.completed" + prev + "data: " + data + prev + blank),
			Event: []byte("response.completed"),
			Data:  []byte(data),
		}
	}
	cases := []struct {
		name string
		ev   sserelay.Event
		want bool
		why  string
	}{
		{"lf", frame("\n", "\n"), true, "LF 完整帧（\\n\\n）"},
		{"crlf", frame("\r\n", "\r\n"), true, "CRLF 完整帧（\\r\\n\\r\\n）"},
		{"crlf-line-lf-blank", frame("\r\n", "\n"), true, "混合：前一行为 CRLF、空行 LF（…}\\r\\n\\n）"},
		{"lf-line-crlf-blank", frame("\n", "\r\n"), true, "混合：前一行为 LF、空行 CRLF（…}\\n\\r\\n）"},
		{"lf-no-blank", frame("\n", ""), false, "LF 残帧：末行完整、缺终止空行"},
		{"crlf-no-blank", frame("\r\n", ""), false, "CRLF 残帧：末行完整、缺终止空行"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isResponsesTerminal(tc.ev), tc.why)
		})
	}
	// 防御面：生产路径 Raw 恒非空，但空 Raw 不得 panic、也不得算终态。
	require.False(t, isResponsesTerminal(sserelay.Event{Data: []byte(respTermDone)}), "Raw 为空不得算终态")
	// 无行终止符的残尾（relay.go:264-272 的 rb.rest() 形态：一个 \n 都没有）—
	// 必须判未终态。tailNoTerm 的 helper 能力此前只有 e2e 覆盖，这里补结构面。
	require.False(t, isResponsesTerminal(frameEvent("response.completed", data, tailNoTerm)),
		"无行终止符的残尾不得算终态")
}

// TestIsResponsesTerminalUnterminatedIsTightening 有意收紧（spec §3 坑 2 / §4.1）：
// 终态帧的 `data:` 行已完整写出、载荷本身也是完整合法的终态 JSON，**只缺结尾空行**
// 时必须判「未终态」——这不是漏判。relay 原样写 frame.Bytes()，客户端收到的字节
// 同样没有结尾空行，多数 SSE 解析器不会派发这个未终止事件 ⇒ 网关必须按未终态收口
// 并补一条真终态帧（旧口径下这种流既不补终态又记 success，客户端最后报
// "stream ended before a terminal response event"）。
func TestIsResponsesTerminalUnterminatedIsTightening(t *testing.T) {
	ev := frameEvent("response.completed",
		`{"type":"response.completed","response":{"id":"r","status":"completed"}}`, tailLineOnly)
	// 两条前提：载荷合法、帧确实缺空行。缺任何一条，本用例就退化成「半截 JSON」。
	require.True(t, json.Valid(ev.Data), "前提：载荷是完整合法的终态 JSON，本用例锁的是帧完整性而非载荷合法性")
	require.False(t, strings.HasSuffix(string(ev.Raw), "\n\n"), "前提：帧确实缺结尾空行")
	require.False(t, isResponsesTerminal(ev), "缺空行的终态帧必须按未终态收口（有意收紧，不是漏判）")
}

// TestEndsWithBlankLineDegenerate 长度守卫（n>=2 / n>=3）的边界表：momus 的
// n>=1 / n>=4 错位与下标写反变异此前全绿，其中两条还真会 panic。逐条 t.Run，
// 让 panic 只打红自己那一格。
func TestEndsWithBlankLineDegenerate(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty", "", false},
		{"lf", "\n", false},
		{"cr", "\r", false},
		{"plain", "a", false},
		{"crlf", "\r\n", false},
		{"lf-cr", "\n\r", false},
		{"blank-lf", "\n\n", true},
		{"text-crlf", "a\r\n", false},
		{"blank-crlf", "\r\n\r\n", true},
		{"blank-lf-crlf", "\n\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			require.NotPanics(t, func() { got = endsWithBlankLine([]byte(tc.raw)) }, "退化输入不得 panic")
			require.Equal(t, tc.want, got, "raw=%q", tc.raw)
		})
	}
}

// TestIsResponsesTerminalZeroAlloc 热路径纪律：常规帧（非终态）与 1 MB 终态帧都
// 必须零分配——若改成 ev.EventName() 逐帧推断，前两条必红。
//
// 注意 1 MB 那条只锁新判据自身的分配数，**对 json.Valid 没有鉴别力**：go1.27 的
// json.Valid 在该载荷上同样 0 allocs/op（本机实测，代价是 ~0.97 ms 的整帧扫描）。
// 「别再退回整帧校验」由 TestIsResponsesTerminal 的判据边界用例与基准的 ns/op 锁。
func TestIsResponsesTerminalZeroAlloc(t *testing.T) {
	named := frameEvent("response.output_text.delta", respTermDelta, tailComplete)
	require.Zero(t, testing.AllocsPerRun(100, func() { _ = isResponsesTerminal(named) }), "具名常规帧终态嗅探零分配")
	// data-only（缺 event: 行）且 **type 不是首键**：InferEventName 的锚点快路径
	// （typeAnchorPrefix）不命中 ⇒ 回退全量解码，每帧一次分配。若实现改调
	// ev.EventName()，这里必红（首键形态有锚点快路径、不分配，鉴别力不足）。
	dataOnly := frameEvent("", `{"item_id":"msg_1","type":"response.output_text.delta","delta":"hi"}`, tailComplete)
	require.Zero(t, testing.AllocsPerRun(100, func() { _ = isResponsesTerminal(dataOnly) }),
		"data-only 帧终态嗅探零分配（不得走 EventName 推断）")
	// 1 MB 终态帧：旧判据在这里是 ~1 ms 的整帧扫描；空行后缀只看末尾 ≤3 字节。
	huge := frameEvent("response.completed", terminalPayload(1<<20), tailComplete)
	require.Zero(t, testing.AllocsPerRun(20, func() { _ = isResponsesTerminal(huge) }), "1 MB 终态帧终态嗅探零分配")
}

// TestWriteClientStreamErrorSkipsSecondTerminal spec §3.4：已见客户端终态时出口
// 只写 error 帧——一条流两个终态会让客户端与记账自相矛盾。白盒直击 Output
// （生产路径的该分支是防御面：已见终态的读错在 §3.3 已按成功收口）。
func TestWriteClientStreamErrorSkipsSecondTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	out := sserelay.NewOutput(rec, 0, sserelay.OutputOptions{Ctx: context.Background()})
	require.NoError(t, out.Commit())
	defer out.Release()

	writeClientStreamError(out, domain.FormatOpenAIResponses, errResponsesStreamIncomplete, true)
	body := rec.Body.String()
	require.Contains(t, body, "event: error\ndata: ", "error 帧照写（真原因必达）")
	require.Equal(t, 1, strings.Count(body, "event: error"), "error 帧恰一条（泄漏实现会写两条）。body=%s", body)
	require.Equal(t, 1, strings.Count(body, `"type":"server_error"`), "error 载荷恰一份。body=%s", body)
	require.Contains(t, body, "without a terminal response event", "文案取哨兵原文")
	require.NotContains(t, body, "response.failed", "已见终态 ⇒ 不追加第二条终态。body=%s", body)
}

// --- codex 直连（verbatim 透传）同三条 ---

// runCodexResponsesDirect 走 codex 直连车道（PAT 静态凭据 → plan.mapper == nil）。
func runCodexResponsesDirect(t *testing.T, upstream string) (int, string, []*domain.UsageLog) {
	t.Helper()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		upstream, nil, nil, store)
	srv := httptest.NewServer(AIRouter(p))
	t.Cleanup(srv.Close)
	resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, p.rec.Close(context.Background()))
	// codex 夹具的 errlog worker 是**运行中**的（20ms flush）：Close 会等它的 loop
	// ctx 退出（fixture 到 t.Cleanup 才 cancel）⇒ 这里只等它落袋。
	require.Eventually(t, func() bool { return len(uniqRows(logRows(store))) >= 1 },
		3*time.Second, 10*time.Millisecond, "错误明细必须落袋（body=%s）", string(body))
	return resp.StatusCode, string(body), uniqRows(logRows(store))
}

// TestCodexResponsesDirectTruncationAppendsFailedTerminal spec §4-7：codex 直连
// 截断（干净 EOF 无终态）补 failed 并按失败记账。
func TestCodexResponsesDirectTruncationAppendsFailedTerminal(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexRawSSEUpstream(t,
		sseData("response.created", respTermCreated)+sseData("response.output_text.delta", respTermDelta))
	status, body, rows := runCodexResponsesDirect(t, up.URL)

	require.Equal(t, http.StatusOK, status, "头已提交（业务帧已写出）")
	types := sseTopLevelTypes(body)
	require.Contains(t, body, "event: error\ndata: ", "body=%s", body)
	require.Contains(t, body, `"type":"response.failed"`, "body=%s", body)
	require.True(t, isPiTerminal(lastTypedEvent(types)), "events=%v", types)
	require.Equal(t, 1, countType(types, "response.failed"))

	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.Equal(t, 0, rows[0].StatusCode)
}

// TestCodexResponsesDirectCompletedSingleTerminal spec §4-7：codex 直连完成 ⇒ 终态
// 恰一条、无 failed、成功记账。
func TestCodexResponsesDirectCompletedSingleTerminal(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexRawSSEUpstream(t,
		sseData("response.created", respTermCreated)+sseData("response.completed", respTermDone))
	status, body, rows := runCodexResponsesDirect(t, up.URL)

	require.Equal(t, http.StatusOK, status)
	types := sseTopLevelTypes(body)
	require.Equal(t, "response.completed", lastTypedEvent(types), "events=%v", types)
	require.Zero(t, countType(types, "response.failed"))
	require.NotContains(t, body, "event: error")

	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType)
	require.Equal(t, http.StatusOK, rows[0].StatusCode)
}

// TestCodexResponsesDirectErrorAfterTerminalStaysSuccess spec §3.3（codex 直连）：
// 终态之后尾块读错 ⇒ 成功收口、不追加第二终态。
func TestCodexResponsesDirectErrorAfterTerminalStaysSuccess(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexAbortUpstream(t, respTermDone)
	status, body, rows := runCodexResponsesDirect(t, up.URL)

	require.Equal(t, http.StatusOK, status)
	types := sseTopLevelTypes(body)
	require.Equal(t, "response.completed", lastTypedEvent(types), "events=%v body=%s", types, body)
	require.Zero(t, countType(types, "response.failed"))
	require.NotContains(t, body, "event: error")

	require.Len(t, rows, 1)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType, "客户端已收到终态 ⇒ 成功收口")
}

// TestCodexConvertedLaneTerminalComesFromPlan spec §3.2：转换车道的「客户端已见
// 终态」由 plan 提供（terminalDone = mapper.Done），**不**由上游帧协议嗅探兜底
// ——干净 EOF 时该车道的终态责任在 plan（plan.finish 补发），即使 Done 仍为 false
// 也不得按截断记失败。白盒：core 只接受注入 plan，故直接构造一个「终态由 plan
// 自报、finish 为空」的 plan 来钉住这条分工。
func TestCodexConvertedLaneTerminalComesFromPlan(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexRawSSEUpstream(t, sseData("response.created", respTermCreated))
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, nil, store)
	sel := selectCodexAccount(t, p, 10)
	cred := sel.Credential()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	plan := codexStreamPlan{
		// 任意帧原样写出（业务帧 ⇒ Output 已提交）。
		mapper:       func(ev sserelay.Event) ([]byte, bool) { return ev.Raw, false },
		finish:       func() []byte { return nil },
		terminalDone: func() bool { return false },
		logFormat:    domain.FormatOpenAIResponses,
	}
	w := httptest.NewRecorder()
	code, respBody, handled, err := p.streamCodexResponsesCore(req.Context(), w, req, "req-1", 10, time.Now(), sel, "gpt-4o", &cred,
		[]byte(`{"model":"gpt-4o","stream":true}`), plan, domain.CodexResponsesTransportHTTP)
	require.True(t, handled)
	require.Equal(t, http.StatusOK, code, "成功收口（不是失败折返 0）")
	require.Nil(t, respBody)
	require.NoError(t, err)
	require.NotContains(t, w.Body.String(), "response.failed", "转换车道的终态由 plan 负责，不由上游帧嗅探")
	require.NotContains(t, w.Body.String(), "event: error")

	require.NoError(t, p.rec.Close(context.Background()))
	require.Eventually(t, func() bool { return len(uniqRows(logRows(store))) >= 1 },
		3*time.Second, 10*time.Millisecond, "行必须落袋")
	rows := uniqRows(logRows(store))
	require.Equal(t, domain.ErrNone, rows[0].ErrorType, "plan 自报终态 ⇒ 成功（不得按截断记失败）")
}

// TestCodexResponsesTruncationDoesNotAdvanceIdentity R2 评审 must-fix：截断
// （干净 EOF 无终态）**不得推进身份/退休线程**——哨兵判定必须早于
// `if err == nil && usageTaken { sel.StepIdentity() }`。
//
// 为什么用「截断的 completed 帧」：哨兵若晚于 StepIdentity，只有 `usageTaken`
// 为真的截断才会走进推进分支（纯 delta 截断的 usageTaken=false，顺序不可观测）。
// 故上游发 `data: {"type":"response.completed",…, "usage":{…}`（type 与 usage 的
// 字节都在、JSON 不合法）**且不留帧尾**（末行无行终止符 = EOF 残帧，relay.go:446-476
// 的残尾形态）⇒ usageTaken=true 而 isResponsesTerminal=false，正是该顺序的可观测
// 形态。残帧必须真的缺结尾空行：终态判据看帧结构完整性，补上 `\n\n` 就变成
// 「完整帧 + 坏 JSON」，判据会认作已终态，本用例随即失去观测面。
//
// 观测面用行为而不是私有字段：WMax=1 时跨窗即退休换线程，而一个窗口 = 48..96
// 轮（spanHi=96）⇒ 推进 97 次必换线程、不推进则线程/window 恒为首轮。
// 阶段 2 用 97 次成功做**正对照**（证明本观测面确实能动）。
func TestCodexResponsesTruncationDoesNotAdvanceIdentity(t *testing.T) {
	testHealthSink.reset()
	const drives = 97 // > spanHi(96)：任何一次推进都会跨窗（WMax=1 ⇒ 跨窗即退休）
	const truncated = `{"type":"response.completed","response":{"id":"resp_t","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`

	var calls atomic.Int64
	type idObs struct{ thread, window string }
	var mu sync.Mutex
	var obs []idObs
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 入站 body 可能 zstd 压缩（codex 请求面）：用既有解码助手取明文。
		body := readFakeUpstreamBody(r)
		mu.Lock()
		obs = append(obs, idObs{
			thread: gjson.GetBytes(body, "client_metadata.thread_id").String(),
			window: gjson.GetBytes(body, "client_metadata.x-codex-window-id").String(),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		if calls.Add(1) <= drives {
			// 残帧：只写 data 行、不留行终止符/空行（真实 EOF 截断形态）。
			_, _ = io.WriteString(w, "data: "+truncated)
		} else {
			_, _ = io.WriteString(w, "data: "+t6RespCreated+"\n\n"+"data: "+t6RespDone+"\n\n")
		}
		fl.Flush()
	}))
	t.Cleanup(up.Close)

	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store,
		withCodexIdentitySlots(1), // 单槽单会话：thread 只因推进而变化
		withCodexRotatePolicy(sdkbridge.RotatePolicy{WMaxLo: 1, WMaxHi: 1}))
	srv := httptest.NewServer(AIRouter(p))
	t.Cleanup(srv.Close)

	for i := range 2 * drives {
		resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "第 %d 个请求：%s", i, string(body))
	}
	mu.Lock()
	got := append([]idObs(nil), obs...)
	mu.Unlock()
	require.Len(t, got, 2*drives, "每个请求都必须触达上游（拒绝/冷却会让本观测面失效）")
	require.NotEmpty(t, got[0].thread, "槽位路径必须带 thread（身份注入）")

	for i := 0; i < drives; i++ {
		require.Equal(t, got[0].thread, got[i].thread, "第 %d 次截断后的 thread 变了：截断推进了身份（哨兵必须早于 StepIdentity）", i)
		require.Equal(t, got[0].thread+":0", got[i].window, "第 %d 次截断后的 window 变了：截断推进了身份", i)
	}
	// 正对照：成功 97 次必然跨窗（WMax=1 ⇒ 退休换线程），证明观测面有效。
	require.NotEqual(t, got[0].thread, got[len(got)-1].thread,
		"正对照失败：成功推进必须能换线程（否则本观测面对「未推进」没有鉴别力）")

	// errlog worker 在本夹具里是运行中的（Close 要等其 loop ctx，t.Cleanup 才 cancel）：
	// 本用例只断言身份观测面，不读错误明细，故不排空它。
	require.NoError(t, p.rec.Close(context.Background()))
}

// usageRecorder 单测代理的标准用量记录器（BatchSize 大 + 小时级 flush：断言前
// 显式 Close 排空）。
func usageRecorder(store *captureLogStore) *usage.Recorder {
	return usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
}
