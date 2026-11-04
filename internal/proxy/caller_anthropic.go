// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/sserelay"
)

type anthropicCaller struct{ p *Proxy }

// anthropicCaller 负责 Messages 的原始 SSE 中继，缓存用量在 message_start、输出用量在 message_delta 分别提取。

func (c *anthropicCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	if stream {
		// 客户端请求模型：流式不解析完整参数，仅 gjson 提取顶层 model；原始字节中继，上游通过 AnthMessageRaw 直透
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型映射：等价 SDK 路径对 params.Model 的覆盖，客户端已带 stream:true 无需注入，命中则零分配复用原切片
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		resp, err := p.clients.AnthMessageRaw(ctx, sel.TemplateID, sel.BaseURL, pathOf(sel, pathKeyAnthropic, defaultPathAnthropic), cred, streamBody, r.Header)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			// 2xx-非-200 归一 502 再交 pipeline（attempt_outcome 拒绝 ResultFailed+2xx）。
			return streamUpstreamStatus(resp.StatusCode), rb, false, nil
		}
		writeSSEHeaders(w)
		out := sserelay.NewOutput(w, p.cfg.StreamKeepaliveInterval, sserelay.OutputOptions{Ctx: ctx, Cancel: cancel})
		defer out.Release()
		var it, ot, tt, cr, cc int64
		// TTFT 首帧语义：首个 SSE 事件（写出前 seam）记录毫秒，已提交流无帧则 nil
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Output: out,
			Mapper: newResponseModelSSEMapper(sel.ClientResponseModel(reqModel)),
			OnEvent: func(ev sserelay.Event) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 协议用量：input/cache 在 message_start，output 在 message_delta；缺 event 名按 data.type 推断
				switch string(ev.EventName()) {
				case "message_start":
					if t, ok := anthropicStartUsage(ev.Data); ok {
						it, cr, cc = t.it, t.cr, t.cc
					}
				case "message_delta":
					ot = anthropicDeltaOutput(ev.Data)
				}
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		// 观测器恰好一次归属：后续分支仅走 Cancel / 失败 / 成功之一
		base := mergeDispatchBase(ctx, anthropicBaseOutcome(reqID, groupID, sel, reqModel, start, ttft, it, ot, cr, cc))
		if err != nil {
			// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
			switch classifyStreamExit(ctx, out, err, AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}, ttft) {
			case streamExitClientCancel:
				// 已提交流用量保留：沿用断前已收到的用量，无则 0，记 200+ErrAbort 防丢日志
				oc := base
				oc.Result = ResultClientCancel
				oc.Commit = CommitResponseStarted
				oc.HTTPStatus = 0
				oc.Terminal = true
				oc.BusinessFrameSent = true
				p.observeDispatchOutcome(ctx, oc, nil)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			case streamExitUncommitted:
				// 未提交读取失败 → pipeline（handled=false，可 failover/写 JSON）
				return statusOf(err), nil, false, err
			default: // 写失败 / 已提交
				if out.Committed() {
					writeClientStreamError(out, domain.FormatAnthropic, err)
				}
				oc := base
				oc.Result = ResultFailed
				oc.HTTPStatus = 0
				oc.Commit = CommitSentAmbiguous
				oc.Terminal = true
				oc.BusinessFrameSent = true
				health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(statusOf(err)), ErrorMessage: err.Error()}
				p.observeDispatchOutcome(ctx, oc, health)
				if p.log != nil {
					p.log.Warn("upstream stream aborted", logx.String("request_id", reqID))
				}
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			}
		}
		tt = it + ot
		base.Usage.OutputTokens = ot
		base.Usage.InputTokens = it
		base.Timing.TTFTMS = ttft
		base.Timing.LatencyMS = time.Since(start).Milliseconds()
		oc := base
		oc.Result = ResultSuccess
		oc.HTTPStatus = 200
		oc.Commit = CommitClientCommitted
		oc.Terminal = true
		oc.BusinessFrameSent = true
		oc.Usage = AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		p.observeDispatchOutcome(ctx, oc, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatAnthropic, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}
	reqModel := gjson.GetBytes(body, "model").String()
	if err := validateNonStreamBody(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		sel.Release()
		return 400, nil, true, nil
	}
	upBody, setErr := setModel(body, sel.Model)
	if setErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + setErr.Error()}})
		sel.Release()
		return 400, nil, true, nil
	}
	// 非流式长任务上限：anthropic SDK 在客户端本地拒绝 expectedTime > 10min 或
	// 命中模型级上限的非流请求（client.go:307-317 + shared/constant/constants.go）。
	// 去 SDK 后必须显式保留，否则原先本地 400 的请求会打到上游并可挂满上游超时。
	if anthropicRequiresStreaming(sel.Model, gjson.GetBytes(body, "max_tokens").Int()) {
		// 交 pipeline 按 4xx 归一（落 err_logs + 原样透出错误体），不在此处直接写出。
		return 400, []byte(`{"error":{"message":"` + anthropicStreamingRequiredText + `","type":"upstream_error"}}`), false, nil
	}
	data, code, errBody, err := p.rawJSON(ctx, sel, pathOf(sel, pathKeyAnthropic, defaultPathAnthropic), "x-api-key", cred, upBody, withProtocolDefaults(r.Header, true))
	if err != nil {
		return statusOf(err), nil, false, err
	}
	if code != 0 {
		return code, errBody, false, nil
	}
	if m := sel.ClientResponseModel(reqModel); m != "" {
		data = rewriteResponseModelJSON(data, m)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	it, ot, tt, cr, cc := anthropicUsageFromResponse(data)
	base := mergeDispatchBase(ctx, anthropicBaseOutcome(reqID, groupID, sel, reqModel, start, nil, it, ot, cr, cc))
	out := base
	out.Result = ResultSuccess
	out.HTTPStatus = 200
	out.Commit = CommitClientCommitted
	out.Terminal = true
	out.BusinessFrameSent = true
	out.Usage = AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	p.observeDispatchOutcome(ctx, out, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatAnthropic, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
}

func anthropicBaseOutcome(reqID string, groupID int64, sel *scheduler.Selection, reqModel string, start time.Time, ttft *int64, it, ot, cr, cc int64) AttemptOutcome {
	routeID, _ := domain.RouteClassID(groupID, domain.FormatAnthropic, reqModel, domain.OpAnthropicMessages)
	qualityID, _ := domain.QualityClassID(domain.CallerAnthropic, domain.FormatAnthropic, sel.Model, domain.OpAnthropicMessages)
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = hex.EncodeToString(routeID[:8])
	}
	return buildOutcome(CallerAnthropic, OperationTag(domain.OpAnthropicMessages), outcomeParams{
		reqID:          AttemptID(reqID),
		routeClassID:   RouteClassID(hex.EncodeToString(routeID[:])),
		qualityClassID: QualityClassID(hex.EncodeToString(qualityID[:])),
		fingerprint:    CandidateFingerprint(fp),
		templateID:     sel.TemplateID,
		accountID:      sel.AccountID,
		requestedModel: reqModel,
		mappedModel:    sel.Model,
		timing:         AttemptTiming{LatencyMS: max(time.Since(start).Milliseconds(), 0), TTFTMS: ttft},
		usage:          AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc},
	})
}
