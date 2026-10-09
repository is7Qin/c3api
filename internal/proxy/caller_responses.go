// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/sserelay"
)

type responsesCaller struct{ p *Proxy }

// responsesCaller 负责 Responses 原始字节转发、response.completed 用量提取和图像调用计数。

func (c *responsesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	// 按凭证类型分流：codex 凭证走适配层（合成与 SSE 透传），其余走原始字节透传；分流在图像剥离之前，codex 分支不剥离
	if sel.CredentialType.IsCodex() {
		return p.callCodexResponses(ctx, w, r, reqID, groupID, start, sel, body, stream)
	}
	// 图像工具剥离：受模板开关门控，内部先对 "image" 子串预筛，无命中零解析直接透传，命中才最小解析改写
	if sel.StripImageTools {
		body = stripImageTools(body)
	}
	if stream {
		// 客户端请求模型：流式不解析完整参数，仅 gjson 提取顶层 model；原始字节中继，上游通过 ResponseStreamRaw 直透
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型映射：等价 SDK 路径对 params.Model 的覆盖，客户端已带 stream:true 无需注入，命中则零分配复用原切片
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		resp, err := p.clients.ResponseStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody, r.Header)
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
		var img int64 // 图像调用计数旁路，仅 completed 帧最终覆盖
		// TTFT 首帧语义：首个 SSE 事件（写出前 seam）记录毫秒，已提交流无帧则保持 nil
		var ttft *int64
		// 异步续接（§3.1：统一写出前 seam）：首个有效响应 id 帧在写出前快照入队
		// 一次。未装配（store/worker nil）不加逻辑，保持零行为变化。
		var contEnqueued bool
		mapper := newResponseModelSSEMapper(sel.ClientResponseModel(reqModel))
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Output: out,
			Mapper: mapper,
			OnEvent: func(ev sserelay.Event) {
				// 统一写出前采样 seam：TTFT/usage/图像计数 + 续接入队同点。
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 协议用量：仅 response.completed 事件携带 usage，缺 event 名时按 data.type 推断
				if bytes.Equal(ev.EventName(), []byte("response.completed")) {
					if t, ok := responsesCompletedUsage(ev.Data); ok {
						it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
					}
					// 图像检测旁路：completed 帧恒在流末，最终计数覆盖
					if respImageDetectOn(sel) {
						img = respImageCountCompleted(ev.Data)
					}
				}
				if !contEnqueued && p.contBindWired() {
					if id := contFrameID(ev.Data); id != "" {
						p.contEnqueue(ctx, contProtocolREST, id, groupID)
						contEnqueued = true
					}
				}
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		// 观测器恰好一次归属：后续分支仅走 Cancel / 失败 / 成功之一
		base := mergeDispatchBase(ctx, responsesBaseOutcome(reqID, groupID, sel, reqModel, start, ttft, it, ot, tt, cr, cc, img))
		if err != nil {
			// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
			switch classifyStreamExit(ctx, out, err, AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc, CallCount: img}, ttft) {
			case streamExitClientCancel:
				// 已提交流用量保留：沿用断前已收到的 usage 帧，无则 0，记 200+ErrAbort 防丢日志
				oc := base
				oc.Result = ResultClientCancel
				oc.Commit = CommitResponseStarted
				oc.HTTPStatus = 0
				oc.Terminal = true
				oc.BusinessFrameSent = true
				p.observeDispatchOutcome(ctx, oc, nil)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
				return 0, nil, true, nil
			case streamExitUncommitted:
				// 未提交读取失败 → pipeline（handled=false，可 failover/写 JSON）；缓冲残余丢弃。
				return statusOf(err), nil, false, err
			default: // 写失败 / 已提交
				if out.Committed() {
					writeClientStreamError(out, err)
				}
				// 上游流中止：同样保留已收集用量，按连接级/5xx 分类
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
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
				return 0, nil, true, nil
			}
		}
		oc := base
		oc.Result = ResultSuccess
		oc.HTTPStatus = 200
		oc.Commit = CommitClientCommitted
		oc.Terminal = true
		oc.BusinessFrameSent = true
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		p.observeDispatchOutcome(ctx, oc, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
		return 200, nil, true, nil
	}
	var params responses.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝：参数校验失败，已占并发槽仅释放不计健康与用量
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		sel.Release()
		return 400, nil, true, nil
	}
	// 客户端请求模型快照：覆盖前取值用于日志与观测，零额外分配
	reqModel := params.Model
	params.Model = responses.ResponsesModel(sel.Model)
	tpl := tplOf(sel) // 非流式走 SDK 模板路径
	resp, err := p.clients.Response(ctx, tpl, cred, params, r.Header)
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
	var it, ot, tt, cr, cc int64
	var img int64 // 图像调用计数旁路
	if resp.JSON.Usage.Valid() {
		// 非流式用量：Responses 的 cache_creation 恒为 0 预期，直接读取 usage，cr/cc 同步落库
		it, ot, tt, cr, cc = responsesUsageFromResponse(resp.Usage)
	}
	// 响应侧图像检测旁路：基于 SDK 保留的上游原始 RawJSON，与缓存用量同款 raw 消费路径，受开关门控
	if respImageDetectOn(sel) {
		img = respImageCountBody([]byte(resp.RawJSON()))
	}
	base := mergeDispatchBase(ctx, responsesBaseOutcome(reqID, groupID, sel, string(reqModel), start, nil, it, ot, tt, cr, cc, img))
	// ACK-before-visible：响应 id 在 Redis 绑定确认前不得写出（store 未装配
	// 零开销）。绑定失败/冲突 → fail-closed：响应弃置，客户端只见归一错误。
	if p.cont != nil {
		if id := gjson.GetBytes(data, "id").String(); id != "" {
			if ferr := p.contBind(ctx, contProtocolREST, id, groupID); ferr != nil {
				out := base
				out.Result = ResultFailed
				out.HTTPStatus = AttemptStatus(ferr.status)
				out.Commit = CommitUpstreamResponded
				out.Terminal = true
				p.observeDispatchOutcome(ctx, out, nil)
				writeErr(w, ferr)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, string(reqModel), sel.LogMappedModel(string(reqModel)), domain.FormatOpenAIResponses, ferr.status, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
				return ferr.status, nil, true, nil
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	out := base
	out.Result = ResultSuccess
	out.HTTPStatus = 200
	out.Commit = CommitClientCommitted
	out.Terminal = true
	out.BusinessFrameSent = true
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	p.observeDispatchOutcome(ctx, out, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, string(reqModel), sel.LogMappedModel(string(reqModel)), domain.FormatOpenAIResponses, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
	return 200, nil, true, nil
}

func responsesBaseOutcome(reqID string, groupID int64, sel *scheduler.Selection, reqModel string, start time.Time, ttft *int64, it, ot, tt, cr, cc, img int64) AttemptOutcome {
	routeID, _ := domain.RouteClassID(groupID, domain.FormatOpenAIResponses, reqModel, domain.OpResponses)
	qualityID, _ := domain.QualityClassID(domain.CallerResponses, domain.FormatOpenAIResponses, sel.Model, domain.OpResponses)
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = hex.EncodeToString(routeID[:8])
	}
	return buildOutcome(CallerResponses, OperationTag(domain.OpResponses), outcomeParams{
		reqID:          AttemptID(reqID),
		routeClassID:   RouteClassID(hex.EncodeToString(routeID[:])),
		qualityClassID: QualityClassID(hex.EncodeToString(qualityID[:])),
		fingerprint:    CandidateFingerprint(fp),
		templateID:     sel.TemplateID,
		accountID:      sel.AccountID,
		requestedModel: reqModel,
		mappedModel:    sel.Model,
		timing:         AttemptTiming{LatencyMS: max(time.Since(start).Milliseconds(), 0), TTFTMS: ttft},
		usage:          AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc, CallCount: img},
	})
}
