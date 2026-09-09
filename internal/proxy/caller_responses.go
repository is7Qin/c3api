// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	if isCodexCredentialType(sel.CredentialType) {
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
		resp, err := p.clients.ResponseStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
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
		// ACK-before-visible：响应 id 帧在 Redis 绑定确认前不得达客户端（仅
		// store 装配时上闸——未装配零行为变化零分配）。
		var gate *contGateWriter
		var sink http.ResponseWriter = w
		if p.cont != nil {
			gate = &contGateWriter{w: w}
			sink = gate
		}
		var contErr *formatError
		var it, ot, tt, cr, cc int64
		var img int64 // 图像调用计数旁路，仅 completed 帧最终覆盖
		// TTFT 首帧语义：首个 SSE 事件写出后回调记录毫秒，已提交流无帧则保持 nil
		var ttft *int64
		err = sserelay.Relay(ctx, sink, resp.Body, sserelay.Config{
			Mapper: newResponseModelSSEMapper(sel.ClientResponseModel(reqModel)),
			Observer: func(ev sserelay.Event) {
				// 首帧即 TTFT，Observer 在帧写出后触发，最接近客户端感知
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
				if gate != nil && contErr == nil {
					released, failed := gate.gateState()
					if failed {
						// 缓冲上限击穿（id 帧迟迟未现）→ fail-closed 弃流
						contErr = errContUnavailable
					} else if !released {
						if id := contFrameID(ev.Data); id != "" {
							if ferr := p.contBind(ctx, contProtocolREST, id, groupID); ferr != nil {
								gate.markFailed()
								contErr = ferr
							} else {
								// ACK 已回：缓冲帧此刻才对客户端可见。
								if err := gate.release(); err != nil {
									gate.markFailed()
									contErr = errContUnavailable
								}
							}
						}
					}
				}
			},
		})
		resp.Body.Close()
		if err == nil && gate != nil && gate.notReleased() {
			if releaseErr := gate.release(); releaseErr != nil {
				contErr = errContUnavailable
			}
		}
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		// 观测器恰好一次归属：后续分支仅走 Cancel 或 Complete 之一
		base := mergeDispatchBase(ctx, responsesBaseOutcome(reqID, groupID, sel, reqModel, start, ttft, it, ot, tt, cr, cc, img))
		if contErr != nil {
			// 绑定失败/缓冲超限：闸门未放，客户端未见任何字节——错误可达，
			// 已消耗用量按 abort 语义保留计费（与流中止同轨），终态不迁移。
			out := base
			out.Result = ResultFailed
			out.HTTPStatus = AttemptStatus(contErr.status)
			out.Commit = CommitUpstreamResponded
			out.Terminal = true
			out.BusinessFrameSent = false
			p.observeDispatchOutcome(ctx, out, nil)
			writeErr(w, contErr)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, contErr.status, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
			return contErr.status, nil, true, nil
		}
		if err != nil {
			// 客户端取消 vs 上游停滞：Canceled 为客户端断开，DeadlineExceeded 为上游超时，后者走失败分支
			if errors.Is(err, context.Canceled) {
				// 已提交流用量保留：沿用断前已收到的 usage 帧，无则 0，记 200+ErrAbort 防丢日志
				out := base
				out.Result = ResultClientCancel
				out.Commit = CommitResponseStarted
				out.HTTPStatus = 0
				out.Terminal = true
				out.BusinessFrameSent = true
				if gate != nil && gate.notReleased() {
					// 闸门未放 = 客户端实际未见任何帧（not_sent 语义）
					out.Commit = CommitNotSent
					out.BusinessFrameSent = false
				}
				p.observeDispatchOutcome(ctx, out, nil)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
				return 0, nil, true, nil
			}
			if gate != nil && gate.notReleased() {
				// id 帧前上游停滞/断流：无字节可见 → 归一 502 错误可达，终态。
				out := base
				out.Result = ResultFailed
				out.HTTPStatus = http.StatusBadGateway
				out.Commit = CommitUpstreamResponded
				out.Terminal = true
				p.observeDispatchOutcome(ctx, out, nil)
				writeErr(w, &formatError{status: http.StatusBadGateway, msg: "upstream rejected request"})
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, http.StatusBadGateway, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
				return http.StatusBadGateway, nil, true, nil
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
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
			return 0, nil, true, nil
		}
		out := base
		out.Result = ResultSuccess
		out.HTTPStatus = 200
		out.Commit = CommitClientCommitted
		out.Terminal = true
		out.BusinessFrameSent = true
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		p.observeDispatchOutcome(ctx, out, health)
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
	resp, err := p.clients.Response(ctx, tpl, cred, params)
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
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID(hex.EncodeToString(routeID[:])), QualityClassID: QualityClassID(hex.EncodeToString(qualityID[:])), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerResponses, OperationTag: OperationTag(domain.OpResponses),
		Ordinal: 1, Lane: LanePrimary, Generation: 1, LifecycleRevision: 1,
		Timing: AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft},
		Usage:  AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc, CallCount: img},
	}
}
