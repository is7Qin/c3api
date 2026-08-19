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

	"github.com/openai/openai-go"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// chatCaller 是 openai-chat 格式的 UpstreamCaller 实现（从 tryChat 迁移，
// 行为逐行等价）：流式走 SDK 原始请求 + sserelay，非流式走 SDK 参数路径；
// 记录职责全在本实现（finish/buildLog/recordStreamAbort/MarkResult）。
type chatCaller struct{ p *Proxy }

func (c *chatCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p

	if stream {
		// 客户端请求模型：流式无完整 params 解析（评审 I-2），gjson 顶层
		// 提取（1 次分配，远低于旧的完整参数解析）。ChatModel 即 string 别名。
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// SDK NewStreaming 会在请求层注入 "stream": true；原始请求必须显式注入，
		// 否则上游按非流式响应，relay 收不到 SSE。注入后仍需发送原始 body。
		// 模型改写：调度器选号已应用 ModelMapping（sel.Model 为上游模型名），
		// 与 SDK 路径 params.Model = sel.Model 等价，映射配置在流式下不失效。
		// GC 削减 P1/P1b：短路守卫（stream 已是 true 且 model 已匹配 → 原切片
		// 零分配）或单次 map 往返同时改两字段（与旧两次往返字节逐位相同）。
		streamBody, err := setStreamAndModel(body, true, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		resp, err := p.clients.ChatCompletionStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			return resp.StatusCode, rb, false, nil
		}
		// SSE 响应头与旧 sseWriter 一致（relay 只转发字节，不代设头）
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var it, ot, tt, cr, cc int64
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
				// "usage": null 的帧（存在但为 null）不得清零元组：chatStreamUsage
				// 内建 usage 存在性判定（值首字节 {/[；缺失与显式 null 均
				// ok=false → 不更新——与原 gjson Type==JSON 前置检查等价）。
				// 热路径预筛：bytes.Contains 零分配子串预筛 "usage" 键帧（同
				// sniffResponsesCompleted 纪律），误命中回退全量扫描语义不变。
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
			// 客户端断开：释放槽位，无法转移。errors.Is(err, context.Canceled) 即
			// 客户端断开——sserelay.normalize 已区分三类（C-P2-2）：父 ctx 取消 →
			// Canceled；上游停滞超时（UpstreamStreamTimeout）→ DeadlineExceeded，
			// 走上游错误分支（recordStreamAbort + 连接级/5xx 分流），不得当作客户端断开。
			if errors.Is(err, context.Canceled) {
				// 客户端断开：上游已消费请求（成功），仍须记录用量，否则
				// 成功请求丢日志。与上游流中止同语义：200 + ErrAbort。
				p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			}
			p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, err)
			p.sched.MarkResult(sel.AccountID, scheduler.RuleKindOf(statusOf(err)), nil, statusOf(err), err.Error(), sel.Model)
			return 0, nil, true, nil
		}
		p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
		p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}

	var params openai.ChatCompletionNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝（handled=true，无记录）：非流式 params 解析失败现状即
		// 本地 400、不记日志（评审 I-1 附加缺口）。Select 已占并发槽，必须
		// 释放（Release-only；finish(nil) 等价，直接 Release 更显式）。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		p.sched.Release(sel.AccountID)
		return 400, nil, true, nil
	}
	// 客户端请求模型快照：下一行覆盖前取值（零额外分配，与 gjson 值等价）。
	reqModel := params.Model
	params.Model = sel.Model
	tpl := tplOf(sel) // 非流式 SDK 路径（GC 削减 P6：流式原始请求路径已免模板对象分配）
	resp, err := p.clients.ChatCompletion(ctx, tpl, cred, params)
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
	if resp.JSON.Usage.Valid() {
		// 非流式：cr 直读 SDK 结构体、cc 走 RawJSON() 原始字节 gjson 聚合
		// （评审 I-1 方案——结构体 marshal 自证不可用）。
		it, ot, tt, cr, cc = chatUsageFromResponse(resp.Usage)
	}
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
}
