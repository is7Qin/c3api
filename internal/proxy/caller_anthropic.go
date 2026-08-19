// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// anthropicCaller 是 anthropic 格式的 UpstreamCaller 实现（从 tryAnthropic
// 迁移，行为逐行等价）。
type anthropicCaller struct{ p *Proxy }

func (c *anthropicCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p

	if stream {
		// 客户端请求模型：流式无完整 params 解析（评审 I-2），gjson 顶层
		// 提取（1 次分配，远低于旧的完整参数解析）。Model 即 string 别名。
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
		resp, err := p.clients.AnthMessageStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
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
		// TTFT 采集（首 token 时间毫秒）：首个 SSE 帧（任意事件）到达时间——
		// Observer 在帧原样写出后回调；单帧旁路零成本（time.Now 一次 + 毫秒
		// 换算）。首帧后写入 ctx（logWithCtx 读取）；无首 token 路径不写入 → nil。
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Observer: func(ev sserelay.Event) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 真实 API 的流式用量分两处携带：input/cache 在 message_start 事件的
				// message.usage 里（评审 M1：前缀 message.usage.*，非顶层），
				// output_tokens 在 message_delta 事件的 usage 里
				// （message_delta.usage 不含 input_tokens）。EventName：缺 event:
				// 名帧按 data.type 推断（非规范上游，P3）。
				switch string(ev.EventName()) {
				case "message_start":
					// ot/tt 恒 0（anthropicStartUsage 无对应字段；tt 下游自算）
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
		if err != nil {
			// 客户端断开：释放槽位，无法转移。errors.Is(err, context.Canceled) 即
			// 客户端断开——sserelay.normalize 已区分三类（C-P2-2）：父 ctx 取消 →
			// Canceled；上游停滞超时（UpstreamStreamTimeout）→ DeadlineExceeded，
			// 走上游错误分支（recordStreamAbort + 连接级/5xx 分流），不得当作客户端断开。
			if errors.Is(err, context.Canceled) {
				// 客户端断开：上游已消费请求（成功），仍须记录用量，否则
				// 成功请求丢日志。与上游流中止同语义：200 + ErrAbort。
				p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			}
			p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, err)
			p.sched.MarkResult(sel.AccountID, scheduler.RuleKindOf(statusOf(err)), nil, statusOf(err), err.Error(), sel.Model)
			return 0, nil, true, nil
		}
		tt = it + ot
		p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
		p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}

	var params anthropic.MessageNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝（handled=true，无记录）：同 chat 语义。Select 已占并发槽，
		// 必须释放（Release-only）。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		p.sched.Release(sel.AccountID)
		return 400, nil, true, nil
	}
	// 客户端请求模型快照：下一行覆盖前取值（零额外分配，与 gjson 值等价）。
	reqModel := params.Model
	params.Model = sel.Model // Model = string 别名
	tpl := tplOf(sel)        // 非流式 SDK 路径（GC 削减 P6：流式原始请求路径已免模板对象分配）
	resp, err := p.clients.AnthMessage(ctx, tpl, cred, params)
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
		// 非流式：SDK v1.56.0 Usage 结构体直读。
		it, ot, tt, cr, cc = anthropicUsageFromResponse(resp.Usage)
	}
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
}
