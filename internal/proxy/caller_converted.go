// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// convertedCaller 是协议转换路径的 UpstreamCaller（W5）：请求体已由
// handleFormat 按方向转换（route.body，含 stream/model 字段），本实现按模板
// 协议调用上游（与 responsesCaller/anthropicCaller 同构），响应反向转换回
// 客户端协议：
//   - 流式：每帧经 protoconv.StreamMapper 映射后写出（sserelay Mapper；
//     Observer 仍见原始帧 → 用量提取与模板 caller 逐字同构）
//   - 非流式：上游响应 JSON 整体 ConvertResponse 转换
//
// 日志按客户端协议记录（buildLog format 参数 = 客户端格式——客户端视角的
// 请求格式）；用量提取仍按模板协议（上游字节不变）。
type convertedCaller struct {
	p   *Proxy
	dir domain.ProtocolConvert
}

func (c *convertedCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	client, target := clientAndTargetOf(c.dir)
	opTag := convertedOpTag(c.dir)

	if stream {
		reqModel := gjson.GetBytes(body, "model").String()
		observer := newConvertedObserver(p, sel, reqModel, opTag)
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			status := AttemptStatus(0)
			outcome := convertedOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, status, CommitNotSent, false, false, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(0), ErrorMessage: err.Error()}
			_ = observer.Complete(outcome, health)
			return 0, nil, false, err
		}
		var resp *http.Response
		switch target {
		case domain.FormatOpenAIResponses:
			resp, err = p.clients.ResponseStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
		case domain.FormatAnthropic:
			resp, err = p.clients.AnthMessageStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
		}
		if err != nil {
			code := statusOf(err)
			var commit CommitState
			if code == 0 {
				commit = CommitNotSent
			} else {
				commit = CommitUpstreamResponded
			}
			terminal := true
			if code == 429 {
				terminal = false
			}
			if code == 0 {
				terminal = false
			}
			outcome := convertedOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), commit, false, terminal, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: err.Error()}
			_ = observer.Complete(outcome, health)
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			code := resp.StatusCode
			commit := CommitUpstreamResponded
			terminal := true
			if code == 429 {
				terminal = false
			}
			outcome := convertedOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), commit, false, terminal, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: string(rb)}
			_ = observer.Complete(outcome, health)
			return resp.StatusCode, rb, false, nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		mapper := protoconv.NewStreamMapper(c.dir)
		var it, ot, tt, cr, cc int64
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Mapper: func(ev sserelay.Event) ([]byte, bool) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				switch target {
				case domain.FormatOpenAIResponses:
					if bytes.Equal(ev.EventName(), []byte("response.completed")) {
						if t, ok := responsesCompletedUsage(ev.Data); ok {
							it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
						}
					}
				case domain.FormatAnthropic:
					switch string(ev.EventName()) {
					case "message_start":
						if t, ok := anthropicStartUsage(ev.Data); ok {
							it, cr, cc = t.it, t.cr, t.cc
						}
					case "message_delta":
						ot = anthropicDeltaOutput(ev.Data)
					}
				}
				return mapper.Map(string(ev.Event), ev.Data)
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		u := usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}
		usage := AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				outcome := convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultClientCancel, 0, CommitResponseStarted, true, true, false)
				_ = observer.Cancel(outcome)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, http.StatusOK, domain.ErrAbort, u, start)))
				return 0, nil, true, nil
			}
			code := statusOf(err)
			commit := CommitUpstreamResponded
			business := false
			if code == 0 {
				commit = CommitSentAmbiguous
				business = true
			}
			outcome := convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultFailed, AttemptStatus(code), commit, business, true, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: err.Error()}
			_ = observer.Complete(outcome, health)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
		}
		tt = it + ot
		usage = AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		timing = AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft}
		outcome := convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		_ = observer.Complete(outcome, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}

	reqModel := gjson.GetBytes(body, "model").String()
	var data []byte
	var it, ot, tt, cr, cc int64
	tpl := tplOf(sel)
	var upstreamErr error
	switch target {
	case domain.FormatOpenAIResponses:
		var params responses.ResponseNewParams
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
			sel.Release()
			return 400, nil, true, nil
		}
		params.Model = responses.ResponsesModel(sel.Model)
		var resp *responses.Response
		resp, upstreamErr = p.clients.Response(ctx, tpl, cred, params)
		if upstreamErr == nil {
			data, upstreamErr = json.Marshal(resp)
			if resp.JSON.Usage.Valid() {
				it, ot, tt, cr, cc = responsesUsageFromResponse(resp.Usage)
			}
		}
	case domain.FormatAnthropic:
		var params anthropic.MessageNewParams
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
			sel.Release()
			return 400, nil, true, nil
		}
		params.Model = sel.Model
		var resp *anthropic.Message
		resp, upstreamErr = p.clients.AnthMessage(ctx, tpl, cred, params)
		if upstreamErr == nil {
			data, upstreamErr = json.Marshal(resp)
			if resp.JSON.Usage.Valid() {
				it, ot, tt, cr, cc = anthropicUsageFromResponse(resp.Usage)
			}
		}
	}
	if upstreamErr != nil {
		observer := newConvertedObserver(p, sel, reqModel, opTag)
		code := statusOf(upstreamErr)
		var commit CommitState
		if code == 0 {
			commit = CommitNotSent
		} else {
			commit = CommitUpstreamResponded
		}
		terminal := true
		if code == 429 {
			terminal = false
		}
		if code == 0 {
			terminal = false
		}
		outcome := convertedOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), commit, false, terminal, false)
		health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: upstreamErr.Error()}
		_ = observer.Complete(outcome, health)
		return statusOf(upstreamErr), upstreamBody(upstreamErr), false, upstreamErr
	}
	conv, err := protoconv.ConvertResponse(data, c.dir)
	if err != nil {
		observer := newConvertedObserver(p, sel, reqModel, opTag)
		usage := AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		outcome := convertedOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, usage, ResultFailed, 500, CommitUpstreamResponded, false, true, true)
		health := &AttemptHealthEvent{Kind: rule.Kind5xx, ErrorMessage: err.Error()}
		_ = observer.Complete(outcome, health)
		return http.StatusInternalServerError, nil, false, fmt.Errorf("protocol response conversion failed: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(conv)
	observer := newConvertedObserver(p, sel, reqModel, opTag)
	usage := AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
	timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}
	outcome := convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	_ = observer.Complete(outcome, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
}

func convertedOpTag(dir domain.ProtocolConvert) OperationTag {
	client, _ := clientAndTargetOf(dir)
	switch client {
	case domain.FormatOpenAIChat:
		return OperationTag(domain.OpChatCompletions)
	case domain.FormatAnthropic:
		return OperationTag(domain.OpAnthropicMessages)
	case domain.FormatOpenAIResponses:
		return OperationTag(domain.OpResponses)
	default:
		return OperationTag(domain.OpChatCompletions)
	}
}

func newConvertedObserver(p *Proxy, sel *scheduler.Selection, reqModel string, op OperationTag) *AttemptObserver {
	mark := func(o AttemptOutcome, e AttemptHealthEvent) {
		p.sched.MarkResult(o.AccountID, e.Kind, e.ResetAt, int(o.HTTPStatus), e.ErrorMessage, o.MappedModel)
	}
	return NewAttemptObserver(nil, mark, nil, sel.Release)
}

func convertedOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = "fp-" + reqID
	}
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID("rc-" + reqID), QualityClassID: QualityClassID("qc-" + reqID), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerConverted, OperationTag: op, Ordinal: 1, LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: commit, Result: result, HTTPStatus: status, Timing: timing, Usage: usage,
		BusinessFrameSent: businessSent, Terminal: terminal, IsMalformed: malformed,
	}
}

// clientAndTargetOf 转换方向的客户端/模板协议格式（方向合法性由 W1 枚举校验
// 保证；未知方向 → 客户端=模板=零值，仅防御）。
func clientAndTargetOf(dir domain.ProtocolConvert) (domain.RequestFormat, domain.RequestFormat) {
	switch dir {
	case domain.ProtocolConvertChatToResp:
		return domain.FormatOpenAIChat, domain.FormatOpenAIResponses
	case domain.ProtocolConvertMessToResp:
		return domain.FormatAnthropic, domain.FormatOpenAIResponses
	case domain.ProtocolConvertRespToMess:
		return domain.FormatOpenAIResponses, domain.FormatAnthropic
	case domain.ProtocolConvertChatToMess:
		return domain.FormatOpenAIChat, domain.FormatAnthropic
	}
	return "", ""
}
