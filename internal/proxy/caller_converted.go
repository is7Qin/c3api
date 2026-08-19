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

	if stream {
		// 客户端请求模型：转换器保证 model 字段原样保留（补差映射），gjson
		// 顶层提取与模板 caller 同构。
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型改写（ModelMapping 语义）：转换后请求体已含 stream:true（转换器
		// 映射客户端 stream 标志），setModel 短路守卫与模板 caller 同构。
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
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
		mapper := protoconv.NewStreamMapper(c.dir)
		var it, ot, tt, cr, cc int64
		// TTFT 采集（首 token 时间毫秒）：与模板 caller 同构。
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Mapper: func(ev sserelay.Event) ([]byte, bool) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 用量提取走原始帧（与模板 caller 逐字同构；映射只影响写出字节）。
				// EventName：缺 event: 名帧按 data.type 推断（非规范上游，P3）。
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
		if err != nil {
			// 客户端断开/流中止语义与模板 caller 逐字同构（recordStreamAbort +
			// MarkResult；客户端断开 finish ErrAbort 不转移）。errors.Is(err,
			// context.Canceled) 即客户端断开——sserelay.normalize 已区分三类
			// （C-P2-2）：上游停滞超时 → DeadlineExceeded 走上游错误分支。
			if errors.Is(err, context.Canceled) {
				p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			}
			p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, err)
			p.sched.MarkResult(sel.AccountID, scheduler.RuleKindOf(statusOf(err)), nil, statusOf(err), err.Error(), sel.Model)
			return 0, nil, true, nil
		}
		tt = it + ot
		p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
		p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}

	// 非流式：以模板协议参数解析（转换后请求体）→ SDK 调用 → 响应 JSON 反向
	// 转换回客户端协议。reqModel 在参数覆盖前从转换体提取（model 原样保留）。
	reqModel := gjson.GetBytes(body, "model").String()
	var data []byte
	var it, ot, tt, cr, cc int64
	tpl := tplOf(sel) // 非流式 SDK 路径（流式原始请求路径免模板对象分配）
	var upstreamErr error
	switch target {
	case domain.FormatOpenAIResponses:
		var params responses.ResponseNewParams
		if err := json.Unmarshal(body, &params); err != nil {
			// 本地拒绝（handled=true，无记录）：同 chat caller 语义。
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
			p.sched.Release(sel.AccountID)
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
			p.sched.Release(sel.AccountID)
			return 400, nil, true, nil
		}
		params.Model = sel.Model // Model = string 别名
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
		return statusOf(upstreamErr), upstreamBody(upstreamErr), false, upstreamErr
	}
	conv, err := protoconv.ConvertResponse(data, c.dir)
	if err != nil {
		// 响应转换失败 = 网关内部错误（转换器 bug/上游异常字节）；按 500 返回，
		// 骨架按 code>=500 转移（重试同 bug 无益，但语义与现状 5xx 一致）。
		return http.StatusInternalServerError, nil, false, fmt.Errorf("protocol response conversion failed: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(conv)
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
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
