// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// chatCaller 是 openai-chat 格式的 UpstreamCaller 实现（从 tryChat 迁移，
// 行为逐行等价）：流式走 SDK 原始请求 + sserelay，非流式走网关自己的 rawJSON
// （去 SDK，见 forward.go）；记录职责全在本实现
// （finish/buildLog/recordStreamAbort/MarkResult）。
type chatCaller struct{ p *Proxy }

func (c *chatCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p

	if stream {
		// 客户端请求模型：流式无完整 params 解析，gjson 顶层
		// 提取（1 次分配，远低于旧的完整参数解析）。ChatModel 即 string 别名。
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeoutCause(ctx, p.cfg.UpstreamStreamTimeout, errGatewayUpstreamTimeout)
		defer cancel()
		// SDK NewStreaming 会在请求层注入 "stream": true；原始请求必须显式注入，
		// 否则上游按非流式响应，relay 收不到 SSE。注入后仍需发送原始 body。
		// 模型改写：调度器选号已应用 ModelMapping（sel.Model 为上游模型名），
		// 与 SDK 路径 params.Model = sel.Model 等价，映射配置在流式下不失效。
		// GC 削减：短路守卫（stream 已是 true 且 model 已匹配 → 原切片
		// 零分配）或单次 map 往返同时改两字段（与旧两次往返字节逐位相同）。
		streamBody, err := setStreamAndModel(body, true, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		resp, err := p.clients.ChatCompletionRaw(ctx, sel.TemplateID, sel.BaseURL, targetPath(sel, pathKeyChat, defaultPathChat), cred, streamBody, r.Header, sel.Egress.HTTP)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			// 2xx-非-200 归一 502 再交 pipeline（attempt_outcome 拒绝 ResultFailed+2xx）。
			return streamUpstreamStatus(resp.StatusCode), rb, false, nil
		}
		// SSE 响应头与旧 sseWriter 一致（relay 只转发字节，不代设头）
		writeSSEHeaders(w)
		out := sserelay.NewOutput(w, p.cfg.StreamKeepaliveInterval, sserelay.OutputOptions{Ctx: ctx, Cancel: cancel})
		defer out.Release()
		var it, ot, tt, cr, cc int64
		// TTFT 采集（首 token 时间毫秒）：首个 SSE 帧（任意事件）到达时间——
		// 统一写出前 seam 回调（Mapper 之后、写缓冲之前），与客户端感知首 chunk
		// 最接近；单帧旁路零成本（time.Now 一次 + 毫秒换算）。首帧后写入 ctx
		// （logWithCtx 读取）；无首 token 路径（Relay 前失败）不写入 → nil。
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Output: out,
			// 流内错误载荷 = 本流失败（spec-inband-error-truth §3-3）。
			FailOnErrorPayload: true,
			Mapper:             newResponseModelSSEMapper(sel.ClientResponseModel(reqModel)),
			OnEvent: func(ev sserelay.Event) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// "usage": null 的帧（存在但为 null）不得清零元组：chatStreamUsage
				// 内建 usage 存在性判定（值首字节 {/[；缺失与显式 null 均
				// ok=false → 不更新——与原 gjson Type==JSON 前置检查等价）。
				// 热路径预筛：bytes.Contains 零分配子串预筛 "usage" 键帧（同
				// response.completed 嗅探纪律 sniffResponsesCompletedUsage），
				// 误命中回退全量扫描语义不变。
				if len(ev.Event) == 0 && bytes.Contains(ev.Data, []byte(`"usage"`)) {
					if t, ok := chatStreamUsage(ev.Data); ok {
						it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
					}
				}
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		if err != nil {
			// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
			// 客户端断开/上游中断均经 typed outcome 统一收敛，保证 exactly-one 观测。
			base := mergeDispatchBase(ctx, chatDispatchedBase(sel, reqID, reqModel, start))
			exit := classifyStreamExit(ctx, out, err, AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}, ttft)
			if exit == streamExitUncommitted {
				// 未提交读取失败 → pipeline（handled=false，可 failover/写 JSON）；缓冲残余丢弃。
				// 网关预算到期（零帧）给错误打标记：pipeline 归类成 timeout（§3.1）。
				return statusOf(err), nil, false, markGatewayBudget(ctx, err)
			}
			// 已提交 / 写失败：唯一归因只在这里算一次（§3 序 0a–0c + 1–5）。
			f := streamFailureOf(ctx, exit, err)
			// 交付事实快照（必须在补写 SSE error 帧之前）：补写失败会把
			// WriteFailed 由 false 翻成 true，晚读会让同一次故障的取值漂移。
			framesSent, writeFailed := out.BusinessFrameSent(), out.WriteFailed()
			if exit != streamExitClientCancel && out.Committed() {
				writeClientStreamError(out, domain.FormatOpenAIChat, err)
			}
			// 中止/失败都保留已采集的 usage/TTFT（成功请求丢日志防线）。
			outcome := chatOutcomeForAbort(base, framesSent, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, ttft, exit == streamExitClientCancel)
			// 交付事实（唯一折算入口）：framesSent/writeFailed 取快照（sserelay.Output
			// 在补写前的真实事实，不是出口构造的常量），clientCancel 由出口归因派生。
			commit := commitStateFor(framesSent, writeFailed, clientEnded(exit, f))
			_ = p.reportChatOutcome(ctx, outcome, sel, reqID, groupID, reqModel, start, f, commit)
			return 0, nil, true, nil
		}
		// 流式成功：携带 TTFT 与已聚合的 usage 经 typed outcome 统一记录，保证 exactly-one 观测与记录归一。
		base := mergeDispatchBase(ctx, chatDispatchedBase(sel, reqID, reqModel, start))
		outcome := chatOutcomeForSuccess(base, ttft, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc})
		_ = p.reportChatOutcome(ctx, outcome, sel, reqID, groupID, reqModel, start, streamFailure{}, outcome.Commit)
		return 200, nil, true, nil
	}

	reqModel := gjson.GetBytes(body, "model").String()
	if err := validateNonStreamBody(body); err != nil {
		// 本地拒绝（handled=true，无记录）：请求体形状非法记本地 400，不记日志；
		// 已占并发槽必须释放。
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
	data, code, errBody, err := p.rawJSON(ctx, sel, targetPath(sel, pathKeyChat, defaultPathChat), "Authorization", "Bearer "+cred, upBody, r.Header)
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
	it, ot, tt, cr, cc := chatUsageFromResponse(data) // 缺失/null → 全 0
	// 非流式成功经 typed outcome 统一收敛，保证 exactly-one 观测与记录归一。
	base := mergeDispatchBase(ctx, chatDispatchedBase(sel, reqID, reqModel, start))
	outcome := chatOutcomeForSuccess(base, nil, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc})
	_ = p.reportChatOutcome(ctx, outcome, sel, reqID, groupID, reqModel, start, streamFailure{}, outcome.Commit)
	return 200, nil, true, nil
}
