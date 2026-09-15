// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
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
		// 客户端请求模型：流式不解析完整参数，仅 gjson 提取顶层 model；原始字节中继，上游通过 AnthMessageStreamRaw 直透
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型映射：等价 SDK 路径对 params.Model 的覆盖，客户端已带 stream:true 无需注入，命中则零分配复用原切片
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		resp, err := p.clients.AnthMessageStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody, r.Header)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			return resp.StatusCode, rb, false, nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var it, ot, tt, cr, cc int64
		// TTFT 首帧语义：首个 SSE 事件写出后回调记录毫秒，已提交流无帧则保持 nil
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Mapper: newResponseModelSSEMapper(sel.ClientResponseModel(reqModel)),
			Observer: func(ev sserelay.Event) {
				// 首帧即 TTFT，Observer 在帧写出后触发，最接近客户端感知
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
		// 观测器恰好一次归属：后续分支仅走 Cancel 或 Complete 之一
		base := mergeDispatchBase(ctx, anthropicBaseOutcome(reqID, groupID, sel, reqModel, start, ttft, it, ot, cr, cc))
		if err != nil {
			// 客户端取消 vs 上游停滞：Canceled 为客户端断开，DeadlineExceeded 为上游超时，后者走失败分支
			if errors.Is(err, context.Canceled) {
				// 已提交流用量保留：沿用断前已收到的用量，无则 0，记 200+ErrAbort 防丢日志
				out := base
				out.Result = ResultClientCancel
				out.Commit = CommitResponseStarted
				out.HTTPStatus = 0
				out.Terminal = true
				out.BusinessFrameSent = true
				p.observeDispatchOutcome(ctx, out, nil)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			}
			// 上游流中止：同样保留已收集用量，按连接级/5xx 分类
			out := base
			out.Result = ResultFailed
			out.HTTPStatus = 0
			out.Commit = CommitSentAmbiguous
			out.Terminal = true
			out.BusinessFrameSent = true
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(statusOf(err)), ErrorMessage: err.Error()}
			p.observeDispatchOutcome(ctx, out, health)
			if p.log != nil {
				p.log.Warn("upstream stream aborted", logx.String("request_id", reqID))
			}
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
			return 0, nil, true, nil
		}
		tt = it + ot
		base.Usage.OutputTokens = ot
		base.Usage.InputTokens = it
		base.Timing.TTFTMS = ttft
		base.Timing.LatencyMS = time.Since(start).Milliseconds()
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
	var params anthropic.MessageNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝：参数校验失败，已占并发槽仅释放不计健康与用量
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		sel.Release()
		return 400, nil, true, nil
	}
	// 客户端请求模型快照：覆盖前取值用于日志与观测，零额外分配
	reqModel := params.Model
	params.Model = sel.Model
	tpl := tplOf(sel) // 非流式走 SDK 模板路径
	resp, err := p.clients.AnthMessage(ctx, tpl, cred, params)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return 0, nil, false, err
	}
	if m := sel.ClientResponseModel(reqModel); m != "" {
		data = rewriteResponseModelJSON(data, m)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var it, ot, tt, cr, cc int64
	if resp.JSON.Usage.Valid() {
		// 非流式用量：直接读取响应 usage，输入/缓存与输出同库
		it, ot, tt, cr, cc = anthropicUsageFromResponse(resp.Usage)
	}
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
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID(hex.EncodeToString(routeID[:])), QualityClassID: QualityClassID(hex.EncodeToString(qualityID[:])), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerAnthropic, OperationTag: OperationTag(domain.OpAnthropicMessages),
		Ordinal: 1, Lane: LanePrimary, Generation: 1, LifecycleRevision: 1,
		Timing: AttemptTiming{LatencyMS: max(time.Since(start).Milliseconds(), 0), TTFTMS: ttft},
		Usage:  AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc},
	}
}
