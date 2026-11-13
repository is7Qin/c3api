// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// convertedCaller 是协议转换路径的 UpstreamCaller：请求体已由
// handleFormat 按方向转换（route.body，含 stream/model 字段），本实现按模板
// 协议调用上游（与 responsesCaller/anthropicCaller 同构），响应反向转换回
// 客户端协议：
//   - 流式：每帧经 protoconv.StreamMapper 映射后写出（sserelay Mapper；
//     OnEvent 仍见原始帧 → 用量提取与模板 caller 逐字同构）
//   - 非流式：上游响应 JSON 整体 ConvertResponse 转换
//
// 日志按客户端协议记录（buildLog format 参数 = 客户端格式——客户端视角的
// 请求格式）；用量提取仍按模板协议（上游字节不变）。
type convertedCaller struct {
	p   *Proxy
	dir domain.ProtocolConvert
}

// streamSupported 流式能力查询的间接层（测试可替换）：未接线的方向必须在这里
// fail-closed 成 400，而不是放行后给客户端一个 200 + 空流。
var streamSupported = protoconv.StreamSupported

// isChatSource 方向的上游是否为 chat（即“上游发的帧是 chat chunk”）。
// 仅两个新方向成立；既有四方向均为 false ⇒ 它们完全不进 chat 相关分支，
// 字节级行为零变化。
func isChatSource(dir domain.ProtocolConvert) bool {
	return dir == domain.ProtocolConvertMessToChat || dir == domain.ProtocolConvertRespToChat
}

func (c *convertedCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	client, target := clientAndTargetOf(c.dir)
	opTag := convertedOpTag(c.dir)
	// 转换路径按目标上游协议记录用量和质量，响应再反向转换回客户端协议。
	// codex 凭证分流：目标协议 = openai-responses 且账号为 codex 类型 → 上游走
	// codex SDK 适配层（不再经通用 aiclient——codex 模板 BaseURL=="" 走 aiclient
	// 会得空 scheme → `unsupported protocol scheme ""`）。分流在 aiclient 分支前。
	if target == domain.FormatOpenAIResponses && sel.CredentialType.IsCodex() {
		return p.callConvertedCodexResponses(ctx, w, r, reqID, groupID, start, sel, body, stream, c.dir)
	}

	if stream {
		// 流式 fail-closed 守卫：**必须在写 SSE 头之前**。未接线的流式
		// 方向若放行，客户端会拿到 200 + 空流（StreamMapper.Map 对未注册
		// 方向静默丢帧），比 400 难排查得多。
		if !streamSupported(c.dir) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "streaming is not supported for this protocol conversion"}})
			sel.Release()
			return http.StatusBadRequest, nil, true, nil
		}
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		var resp *http.Response
		switch target {
		case domain.FormatOpenAIResponses:
			resp, err = p.clients.ResponseRaw(ctx, sel.TemplateID, sel.BaseURL, targetPath(sel, pathKeyResponses, defaultPathResponses), cred, streamBody, r.Header, sel.Egress.HTTP)
		case domain.FormatAnthropic:
			resp, err = p.clients.AnthMessageRaw(ctx, sel.TemplateID, sel.BaseURL, targetPath(sel, pathKeyAnthropic, defaultPathAnthropic), cred, streamBody, r.Header, sel.Egress.HTTP)
		case domain.FormatOpenAIChat:
			resp, err = p.clients.ChatCompletionRaw(ctx, sel.TemplateID, sel.BaseURL, targetPath(sel, pathKeyChat, defaultPathChat), cred, streamBody, r.Header, sel.Egress.HTTP)
		}
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
		mapper := protoconv.NewStreamMapper(c.dir)
		var it, ot, tt, cr, cc int64
		// 首帧到达即记录 TTFT（写出前 seam）；用量仍按目标协议原始帧提取。
		var ttft *int64
		clientModel := sel.ClientResponseModel(reqModel)
		// 异步续接（仅 resp→mess 方向）：首个可续接映射结果 id 出现时快照入队
		// 一次。id 由 mapper 显式返回（BindableID），不从映射帧机械解析。
		var contEnqueued bool
		var lastDrop bool
		// 正常 EOF 的终止帧：chat 上游**可能不发 `data: [DONE]`** 就关流（截断、
		// 或某些实现在最后一帧后直接 FIN）。不给客户端补终止帧的后果：mess 侧
		// 永远收不到 message_stop、resp 侧永远收不到 response.completed，客户端
		// 只能靠超时判死。Finish 内部按 m.done 去重 ⇒ 与 [DONE] 恰好二选一。
		//
		// I1：终止帧同样要过 rewriteConvertedFrames —— 它内部是
		// `response.completed.response.model`，不过这里就会被写成**上游**模型名，
		// 而同一流前面的帧（response.created 等）已是客户端模型名，客户端读到的
		// 模型名前后不一致。与 Mapper 分支同一处改写、同一判空。
		var terminal func() []byte
		if isChatSource(c.dir) {
			terminal = func() []byte {
				fin := mapper.Finish()
				if len(fin) == 0 || clientModel == "" {
					return fin
				}
				return rewriteConvertedFrames(fin, clientModel)
			}
		}
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Output:   out,
			Terminal: terminal,
			Mapper: func(ev sserelay.Event) ([]byte, bool) {
				mapped, drop := mapper.Map(string(ev.Event), ev.Data)
				lastDrop = drop
				if drop {
					return nil, true
				}
				if clientModel != "" {
					mapped = rewriteConvertedFrames(mapped, clientModel)
				}
				return mapped, false
			},
			OnEvent: func(ev sserelay.Event) {
				// 采样与续接入队统一到写出前 seam（含 drop 帧仍回调；用量走原始帧）。
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 用量提取走原始帧（与模板 caller 逐字同构；映射只影响写出字节）。
				// 原始 ev.Data 在 StreamMapper 转换前提取，转换后改写不影响用量。
				// EventName：缺 event: 名帧按 data.type 推断（非规范上游）。
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
				case domain.FormatOpenAIChat:
					// chat 用量只在末帧（choices:[] + usage 对象）出现；中间帧的
					// usage 键存在但值为 null —— chatStreamUsage 已内建存在判定。
					if t, ok := chatStreamUsage(ev.Data); ok {
						it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
					}
				}
				// 续接入队（W4）：Codex 凭据在上方已分流到 callConvertedCodexResponses，
				// 此处只有 API key 账号；Codex 不绑定续接。
				if !lastDrop && !contEnqueued && p.contBindWired() && c.dir == domain.ProtocolConvertRespToMess {
					if id := mapper.BindableID(); id != "" {
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
		u := usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}
		usage := AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft}
		if err != nil {
			// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
			exit := classifyStreamExit(ctx, out, err, usage, ttft)
			if exit == streamExitUncommitted {
				// 未提交读取失败 → pipeline（handled=false，可 failover/写 JSON）
				return statusOf(err), nil, false, err
			}
			// 已提交 / 写失败：唯一归因（§3 序 0a–0c + 1–5）。
			f := streamFailureOf(ctx, exit, err)
			// 交付事实快照（必须在补写 SSE error 帧之前）：补写失败会把 WriteFailed
			// 由 false 翻成 true，晚读会让同一次故障的取值漂移。
			framesSent, writeFailed := out.BusinessFrameSent(), out.WriteFailed()
			if exit != streamExitClientCancel && out.Committed() {
				writeClientStreamError(out, client, err)
			}
			// 客户端取消与上游中断区分：取消不计健康惩罚，两者都仍需计费落账。
			var outcome AttemptOutcome
			if exit == streamExitClientCancel {
				outcome = mergeDispatchBase(ctx, convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultClientCancel, 0, CommitResponseStarted, true, true, false))
			} else {
				code := statusOf(err)
				commit := CommitUpstreamResponded
				business := false
				if code == 0 {
					commit = CommitSentAmbiguous
					business = true
				}
				outcome = mergeDispatchBase(ctx, convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultFailed, AttemptStatus(code), commit, business, true, false))
			}
			p.observeDispatchOutcome(ctx, outcome, streamFailureHealth(f))
			l := logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), client, logStatusForFailure(f), f.ET, u, start))
			l.ErrorMessage = f.message()
			// 交付事实（唯一折算入口）：framesSent/writeFailed 取补写前快照，
			// clientCancel 由出口归因派生（0a/0c）。
			p.finish(sel, withCommit(l, commitStateFor(framesSent, writeFailed, clientEnded(exit, f))))
			return 0, nil, true, nil
		}
		tt = it + ot
		usage = AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		timing = AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft}
		outcome := mergeDispatchBase(ctx, convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false))
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		p.observeDispatchOutcome(ctx, outcome, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), client, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}

	reqModel := gjson.GetBytes(body, "model").String()
	var data []byte
	var it, ot, tt, cr, cc int64
	// 注：这里校验的是**转换后**的请求体（caller.go 先 ConvertRequest 再派发）。
	// 客户端原始体里的重复 model 键由转换器按 last-wins 收敛成单键，随后必然经过
	// setModel 改写成路由选定的 sel.Model（两者不同则改写，相同则本就一致），因此
	// 不存在「路由看首个、上游执行末个」的分叉；原生格式则在校验里直接拒掉重复键。
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
	// 转换到 anthropic 的非流请求同样受 anthropic SDK 的非流上限约束（旧实现走
	// clients.AnthMessage，该守卫隐式生效）；去 SDK 后必须显式保留。
	if target == domain.FormatAnthropic && anthropicRequiresStreaming(sel.Model, gjson.GetBytes(body, "max_tokens").Int()) {
		return 400, []byte(`{"error":{"message":"` + anthropicStreamingRequiredText + `","type":"upstream_error"}}`), false, nil
	}
	var path, authHeader, auth string
	switch target {
	case domain.FormatOpenAIResponses:
		path, authHeader, auth = targetPath(sel, pathKeyResponses, defaultPathResponses), "Authorization", "Bearer "+cred
	case domain.FormatAnthropic:
		path, authHeader, auth = targetPath(sel, pathKeyAnthropic, defaultPathAnthropic), "x-api-key", cred
	case domain.FormatOpenAIChat:
		path, authHeader, auth = targetPath(sel, pathKeyChat, defaultPathChat), "Authorization", "Bearer "+cred
	default:
		// 未接线的目标协议：显式拒绕（不得带空 path 去拼 URL）。
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "protocol conversion target is not wired"}})
		sel.Release()
		return http.StatusInternalServerError, nil, true, nil
	}
	data, code, errBody, upstreamErr := p.rawJSON(ctx, sel, path, authHeader, auth, upBody, r.Header)
	if upstreamErr != nil {
		return statusOf(upstreamErr), nil, false, upstreamErr
	}
	if code != 0 {
		return code, errBody, false, nil
	}
	switch target {
	case domain.FormatAnthropic:
		it, ot, tt, cr, cc = anthropicUsageFromResponse(data)
	case domain.FormatOpenAIChat:
		it, ot, tt, cr, cc = chatUsageFromResponse(data)
	default:
		it, ot, tt, cr, cc = responsesUsageFromResponse(data)
	}
	conv, err := protoconv.ConvertResponse(data, c.dir)
	if err != nil {
		return http.StatusInternalServerError, nil, false, fmt.Errorf("protocol response conversion failed: %w", err)
	}
	if rm := sel.ClientResponseModel(reqModel); rm != "" {
		conv = rewriteResponseModelJSON(conv, rm)
	}
	// 同步绑定（W5）：Codex 凭据在上方已分流，Codex 不绑定续接。
	if c.dir == domain.ProtocolConvertRespToMess && p.cont != nil {
		if id := gjson.GetBytes(conv, "id").String(); id != "" {
			if ferr := p.contBind(ctx, contProtocolREST, id, groupID); ferr != nil {
				outcome := mergeDispatchBase(ctx, convertedOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}, ResultFailed, AttemptStatus(ferr.status), CommitUpstreamResponded, false, true, false))
				p.observeDispatchOutcome(ctx, outcome, nil)
				p.finish(sel, withCommit(logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), client, ferr.status, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)), outcome.Commit))
				writeErr(w, ferr)
				return ferr.status, nil, true, nil
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(conv)
	usage := AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
	timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}
	outcome := mergeDispatchBase(ctx, convertedOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false))
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	p.observeDispatchOutcome(ctx, outcome, health)
	// 计费落账与日志由 p.finish 统一收口。
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), client, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
}

// callConvertedCodexResponses 协议转换路径命中 codex 凭证类型 + 目标协议 =
// openai-responses 时的上游调用：镜像 callCodexResponses 的入口前置（501 / ext
// 缺失语义与直连一致），再调 codex resp core——注入协议转换 output（响应经
// protoconv 反向映射回客户端协议 + 客户端模型回填；日志 Format 由 output 自报 =
// clientAndTargetOf 的 client 侧——与转换路径其余分支一致；计费不读 format）。
// outcome 取 codex 口径（CallerCodexHTTP / OpResponses，见 codexDispatchBase）。
func (p *Proxy) callConvertedCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, body []byte, stream bool, dir domain.ProtocolConvert) (int, []byte, bool, error) {
	client, _ := clientAndTargetOf(dir)
	// 客户端请求模型（日志/回填口径）：转换后请求体经 ConvertRequest 保持 model。
	reqModel := gjson.GetBytes(body, "model").String()
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, usageTuple{}, nil))
		o.Result = ResultFailed
		o.HTTPStatus = AttemptStatus(http.StatusNotImplemented)
		o.Commit = CommitUpstreamResponded
		o.Terminal = true
		o.BusinessFrameSent = false
		emitCodexOutcome(ctx, p, sel, o, &AttemptHealthEvent{Kind: rule.Kind5xx, ErrorMessage: errCodexResponsesNotIntegrated.msg})
		sel.Release()
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), client, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexResponsesNotIntegrated.msg)
		writeErr(w, errCodexResponsesNotIntegrated)
		return 0, nil, true, nil
	}
	if sel.Ext == nil {
		// 配置损坏（codex 账号必有 ext 行——快照缺 account_ext 行）：连接级错误
		// 转移（失败文本落盘，耗尽 502 语义）；不上报失效（与直连同语义）。
		return 0, nil, false, errCodexExtMissing
	}
	cred := sel.Credential()
	cm := sel.ClientResponseModel(reqModel)
	// 出站传输钉死 http：转换车道上游跑的是**转换后的 payload**，本轮 WS 臂只
	// 验证原生 responses 请求（sdkbridge 组合用例），未覆盖转换产物；钉 http
	// 让转换车道保持本轮唯一已验证的腿，而不是跟随模板开关走进未验证面。
	// 这也让本车道绕过 callCodexResponses ⇒ 天然不写生效传输头。
	if stream {
		return p.streamCodexResponsesCore(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body, codexConvertedStreamPlan(dir, cm), domain.CodexResponsesTransportHTTP)
	}
	return p.nonstreamCodexResponsesCore(ctx, r, reqID, groupID, start, sel, reqModel, &cred, body, newCodexConvertedOutput(w, dir, cm), domain.CodexResponsesTransportHTTP)
}

// convertedOpTag 转换方向对应的操作标签：由 domain.ProtocolConvert.Client()
// 的客户端协议经本地 client→OperationTag 映射得出（Chat→chat_completions、
// Anthropic→anthropic_messages、Responses→responses、未知/off→chat_completions，
// 与重构前 proxy 单点映射的 op 列逐一等价）。
func convertedOpTag(dir domain.ProtocolConvert) OperationTag {
	switch dir.Client() {
	case domain.FormatAnthropic:
		return OperationTag(domain.OpAnthropicMessages)
	case domain.FormatOpenAIResponses:
		return OperationTag(domain.OpResponses)
	}
	return OperationTag(domain.OpChatCompletions)
}

// clientAndTargetOf 转换方向的客户端/模板协议格式（方向合法性由枚举校验
// 保证；未知方向 → 客户端=模板=零值，仅防御）。
func clientAndTargetOf(dir domain.ProtocolConvert) (domain.RequestFormat, domain.RequestFormat) {
	return dir.Client(), dir.Target()
}

func convertedOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	return syntheticOutcome(CallerConverted, reqID, sel, reqModel, op, timing, usage, result, status, commit, businessSent, terminal, malformed)
}
