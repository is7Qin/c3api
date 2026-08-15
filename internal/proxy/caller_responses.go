// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// responsesCaller 是 openai-responses 格式的 UpstreamCaller 实现（从 tryResponses
// 迁移，行为逐行等价）。
type responsesCaller struct{ p *Proxy }

func (c *responsesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p

	// codex 类型分流（T6 §1）：codex-oauth/codex-pat → 适配层调用（SDK 合成非
	// 流式 Responses / Stream SSE 透传——实现独立文件 codex_responses_http.go）；
	// api_key / responses-special → typed 段（下方原样，零改动）。分流在
	// stripImageTools 之前——codex 分支**不剥离**（用户裁决：strip 仅针对
	// resp/resp-ws 的 responses-special）。
	if isCodexCredentialType(sel.CredentialType) {
		return p.callCodexResponses(ctx, w, r, reqID, groupID, start, sel, body, stream)
	}

	// 图像 tool 剥离（W4；模板级开关，三类型公共能力）：关闭 = 快照布尔读 +
	// 分支零开销；开启 = stripImageTools 内部 "image" 子串预筛，无命中零解析
	// 直转，命中才最小解析改写。剥离在客户端入站帧转发上游前执行（网关能力，
	// 不依赖 SDK）——未来 resp-ws 帧流（response.create 帧）复用同一纯函数
	// （strip_image.go）。
	if sel.StripImageTools {
		body = stripImageTools(body)
	}

	if stream {
		// 客户端请求模型：流式无完整 params 解析（评审 I-2），gjson 顶层
		// 提取（1 次分配，远低于旧的完整参数解析）。ResponsesModel 即 string 别名。
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型改写：与 SDK 路径 params.Model = sel.Model 等价（ModelMapping 语义）。
		// 客户端请求体已带 stream:true（fake 上游按 body["stream"] 分支），无需注入。
		// GC 削减 P1：model 已匹配 → 短路返回原切片零分配。
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
		var it, ot, tt, cr, cc int64
		var img int64 // resp 检测功能调用计数（spec §6 旁路；respImageDetectOn 门控）——落 CallCount
		// TTFT 采集（首 token 时间毫秒）：首个 SSE 帧（任意事件）到达时间——
		// Observer 在帧原样写出后回调，与客户端感知首 chunk 最接近；单帧旁路
		// 零成本（time.Now 一次 + 毫秒换算）。首帧后写入 ctx（logWithCtx 读取）；
		// 无首 token 路径（Relay 前失败）不写入 → nil。
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Observer: func(ev sserelay.Event) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 用量只在 response.completed 事件携带（响应对象的 usage 字段；
				// 评审 M2：流式前缀 response.usage.*）。EventName：缺 event: 名
				// 帧按 data.type 推断（非规范上游，P3——否则该上游下 usage 静默缺失）。
				if bytes.Equal(ev.EventName(), []byte("response.completed")) {
					if t, ok := responsesCompletedUsage(ev.Data); ok {
						it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
					}
					// 响应检测旁路（spec §6；与 resp-ws sniff 同族）：completed
					// 帧恒在流末——最终计数由其覆盖（最后帧语义）。门控关闭
					// （api_key/strip 开）→ 零额外解析。
					if respImageDetectOn(sel) {
						img = respImageCountCompleted(ev.Data)
					}
				}
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		if err != nil {
			// 客户端断开：释放槽位，无法转移。errors.Is(err, context.Canceled) 即
			// 客户端断开——sserelay.normalize 已区分三类（C-P2-2）：父 ctx 取消 →
			// Canceled；上游停滞超时（UpstreamStreamTimeout）→ DeadlineExceeded，
			// 走上游错误分支（recordStreamAbort + ResultError），不得当作客户端断开。
			if errors.Is(err, context.Canceled) {
				// 客户端断开：上游已消费请求（成功），仍须记录用量，否则
				// 成功请求丢日志。与上游流中止同语义：200 + ErrAbort，
				// token 取断前已收到的 usage 帧（无则 0）。
				p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
				return 0, nil, true, nil
			}
			p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, statusOf(err), err.Error())
			return 0, nil, true, nil
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
		p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
		return 200, nil, true, nil
	}

	var params responses.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝（handled=true，无记录）：同 chat 语义。Select 已占并发槽，
		// 必须释放（Release-only）。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		p.sched.Release(sel.AccountID)
		return 400, nil, true, nil
	}
	// 客户端请求模型快照：下一行覆盖前取值（零额外分配，与 gjson 值等价）。
	reqModel := params.Model
	params.Model = responses.ResponsesModel(sel.Model)
	tpl := tplOf(sel) // 非流式 SDK 路径（GC 削减 P6：流式原始请求路径已免模板对象分配）
	resp, err := p.clients.Response(ctx, tpl, cred, params)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return 0, nil, false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var it, ot, tt, cr, cc int64
	var img int64 // resp 检测功能调用计数（spec §6 旁路；respImageDetectOn 门控）——落 CallCount
	if resp.JSON.Usage.Valid() {
		// 非流式：cr 直读 SDK 结构体、cc 走 RawJSON() gjson 聚合
		// （Responses 无 cache_creation 对象，恒 0 预期——M4）。
		it, ot, tt, cr, cc = responsesUsageFromResponse(resp.Usage)
	}
	// 响应侧 image 检测（旁路，spec §6）：raw = SDK 保留的上游原文
	// （RawJSON——与 cacheCreationFromRaw 同款 raw 消费路径；非流式路径本
	// 非零分配（SDK 解析 + marshal），此处 1 次 string→[]byte 转换可接受）。
	if respImageDetectOn(sel) {
		img = respImageCountBody([]byte(resp.RawJSON()))
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
	return 200, nil, true, nil
}
