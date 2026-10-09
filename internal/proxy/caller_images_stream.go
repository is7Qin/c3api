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

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// imageStreamGenerator 流式生图能力，与 sdkbridge.Codex.GenerateImageStream 同签名。
type imageStreamGenerator func(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error

// errStreamWriteFailed 兜底错误：WriteFailed 已置但无保存的 I/O 错时的失败出口用。
var errStreamWriteFailed = errors.New("image stream write failed")

func (p *Proxy) streamImageGeneration(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, params *domain.ImageGenParams, gen imageStreamGenerator) (int, []byte, bool, error) {
	// 首事件前保留 HTTP 错误语义；首事件后只能写 SSE error 帧，计费使用已收集的图片和令牌。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
	defer cancel()

	opTag := OperationTag(domain.OpImagesGenerations)
	if params != nil && len(params.Images) > 0 {
		// edits vs generations identity: request carries images
		opTag = OperationTag(domain.OpImagesEdits)
	}
	if r != nil && r.URL != nil && r.URL.Path != "" {
		if len(r.URL.Path) >= 6 && r.URL.Path[len(r.URL.Path)-6:] == "/edits" {
			opTag = OperationTag(domain.OpImagesEdits)
		} else if len(r.URL.Path) >= 12 && r.URL.Path[len(r.URL.Path)-12:] == "/generations" {
			opTag = OperationTag(domain.OpImagesGenerations)
		}
	}
	// 统一下行 owner：该路径始终关闭通用定时保活（SDK 60s 驱动）；Interval=0
	// 不关 images SDK 自带保活。首事件时序保留：CommitAndFlush → caller 记
	// TTFT → Heartbeat/WriteFrame；无事件成功也提交并 flush。Output 不采 TTFT。
	out := sserelay.NewOutput(w, 0, sserelay.OutputOptions{Ctx: ctx, Cancel: cancel})
	defer out.Release()
	var (
		count int64
		usage *domain.ImageUsage
		ttft  *int64
	)
	commitOnce := func() error {
		if out.Committed() {
			return nil
		}
		if err := out.Commit(); err != nil {
			return err
		}
		if ttft == nil {
			ms := time.Since(start).Milliseconds()
			ttft = &ms
		}
		return nil
	}

	genErr := gen(ctx, cred, params, func(ev domain.ImageStreamEvent) error {
		switch ev.Type {
		case domain.ImageStreamEventKeepalive:
			// SDK 合成 keepalive → 网关统一注释帧（: keepalive\n）。
			if err := commitOnce(); err != nil {
				return err // 传播 Commit/FlushError 失败（不得吞掉）
			}
			return out.Heartbeat()
		case domain.ImageStreamEventCompleted:
			count++
			if ev.Usage != nil {
				usage = ev.Usage
			}
			if err := commitOnce(); err != nil {
				return err // 传播 Commit/FlushError 失败（不得吞掉）
			}
			if _, err := out.WriteFrame(buildCompletedFrame(&ev)); err != nil {
				return err
			}
			// 逐事件 Flush（与原 writeFrame 语义一致，保首字节/事件级延迟）。
			return out.DrainFlush()
		default:
			if p.log != nil {
				p.log.Warn("image stream: unknown event type skipped",
					logx.String("request_id", reqID),
					logx.String("type", string(ev.Type)))
			}
			return nil
		}
	})

	var ii, io int64
	if usage != nil {
		ii, io = usage.InputImageTokens, usage.OutputImageTokens
	}
	// 张数按 completed 事件计数，tokens 取末帧 usage。
	u := usageTuple{ii: ii, io: io, tt: ii + io, calls: count}
	usageObs := AttemptUsage{InputTokens: ii, OutputTokens: io, CallCount: count}
	timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft}

	// 无事件成功也提交并 flush；Commit/FlushError 失败（含零事件路径）须进失败
	// 出口，不得记成功——最终观测前复核 WriteFailed/IOErr。
	if genErr == nil {
		if err := commitOnce(); err != nil {
			genErr = err
		} else {
			// 零事件成功路径：ttft 在 commitOnce 内才固化，而 timing 在此前构建
			//（*int64 指针按值拷贝），须回填否则 TTFT 丢失。
			timing.TTFTMS = ttft
			if out.WriteFailed() {
				genErr = out.IOErr()
				if genErr == nil {
					// 纯防御：WriteFailed()==true 蕴含 IOErr()!=nil（failLocked 是唯一
					// 起点且恒带非 nil 错误），此分支理论上不可达。
					genErr = errStreamWriteFailed
				}
			}
		}
	}

	if genErr != nil {
		// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
		// 出口判定用请求 ctx 识别客户端断开（r.Context 与内部超时 ctx 的取消在
		// 生产路径同源；genErr 未必携带 context.Canceled）。
		switch classifyStreamExit(r.Context(), out, genErr, usageObs, ttft) {
		case streamExitClientCancel:
			// 客户端断开：已收集张数照常计费落账，不 MarkResult（不可转移）。
			outcome := mergeDispatchBase(ctx, imagesStreamOutcome(reqID, sel, reqModel, opTag, timing, usageObs, ResultClientCancel, 0, CommitResponseStarted, true, true, false))
			p.observeDispatchOutcome(ctx, outcome, nil)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), sel.Format, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
		case streamExitUncommitted:
			// 未提交失败 → pipeline（handled=false，可 failover/写 JSON）；缓冲残余丢弃。
			if sdkbridge.IsFatal(genErr) {
				code := streamUpstreamStatus(statusOf(genErr))
				if code == 0 {
					code = http.StatusBadGateway
				}
				return code, upstreamBody(genErr), false, genErr
			}
			return streamUpstreamStatus(statusOf(genErr)), upstreamBody(genErr), false, genErr
		default: // 写失败 / 已提交
			if out.Committed() {
				writeClientStreamError(out, domain.FormatOpenAIImages, genErr)
			}
			code := statusOf(genErr)
			commit := CommitUpstreamResponded
			business := false
			if code == 0 {
				commit = CommitSentAmbiguous
				business = true
			}
			outcome := mergeDispatchBase(ctx, imagesStreamOutcome(reqID, sel, reqModel, opTag, timing, usageObs, ResultFailed, AttemptStatus(code), commit, business, true, false))
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: genErr.Error()}
			p.observeDispatchOutcome(ctx, outcome, health)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), sel.Format, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
		}
	}

	outcome := mergeDispatchBase(ctx, imagesStreamOutcome(reqID, sel, reqModel, opTag, timing, usageObs, ResultSuccess, 200, CommitResponseStarted, true, true, false))
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	p.observeDispatchOutcome(ctx, outcome, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), sel.Format, http.StatusOK, domain.ErrNone, u, start)))
	return http.StatusOK, nil, true, nil
}

func imagesStreamOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	return syntheticOutcome(CallerImagesCodex, reqID, sel, reqModel, op, timing, usage, result, status, commit, businessSent, terminal, malformed)
}

// writeSSEHeaders 设置 SSE 响应头三件套（text/event-stream）——单一 SSE 头
// 助手，beginSSE 与所有响应头三件套站点共用（委托 sserelay.SetSSEHeaders，
// 供 sserelay.Output.Commit 共用同一语义）。仅设置头、不提交状态码：提交
// 时机交由首个 Write 或紧随的显式 WriteHeader 决定，保持「首帧前不提交头」
// 的惰性语义（sserelay 站点首帧前失败仍可失败重分类；需立即提交的调用方
// 自行 WriteHeader(200)）。
func writeSSEHeaders(w http.ResponseWriter) {
	sserelay.SetSSEHeaders(w.Header())
}

// buildCompletedFrame 构造 completed 事件的 SSE 帧：
func buildCompletedFrame(ev *domain.ImageStreamEvent) []byte {
	var b64len int
	if ev.B64JSON != nil {
		b64len = len(*ev.B64JSON)
	}
	const evLine = "event: " + string(domain.ImageStreamEventCompleted) + "\ndata: "
	buf := bytes.NewBuffer(make([]byte, 0, len(evLine)+b64len+96))
	buf.WriteString(evLine)
	buf.WriteString(`{"b64_json":`)
	if ev.B64JSON != nil {
		b64 := *ev.B64JSON
		buf.WriteByte('"')
		buf.WriteString(b64)
		buf.WriteByte('"')
	} else {
		buf.WriteString("null")
	}
	if ev.Usage != nil {
		buf.WriteString(`,"usage":`)
		u, _ := json.Marshal(ev.Usage)
		buf.Write(u)
	}
	buf.WriteString("}\n\n")
	return buf.Bytes()
}

// buildErrorFrame 生成客户端协议 SSE error 帧。载荷按客户端协议（§2.3）：
//   - OpenAI（Chat/Responses/Images）：`event: error` + 顶层 `error` 键
//     （`{"error":{"message":…,"type":"server_error"}}`）——openai-go `ssestream`
//     以此判失败（packages/ssestream/ssestream.go:169/181）；
//   - Anthropic：`event: error` + `{"type":"error","error":{"type":"api_error",
//     "message":…}}`。
//
// 不得只发 `{"message":…}`：SDK 不将其识别为失败，截断会被当作正常结束。
func buildErrorFrame(format domain.RequestFormat, message string) []byte {
	if format == domain.FormatAnthropic {
		buf := bytes.NewBuffer(make([]byte, 0, len("event: error\ndata: ")+len(message)+64))
		buf.WriteString(`event: error` + "\ndata: ")
		buf.WriteString(`{"type":"error","error":{"type":"api_error","message":`)
		m, _ := json.Marshal(message)
		buf.Write(m)
		buf.WriteString("}}\n\n")
		return buf.Bytes()
	}
	buf := bytes.NewBuffer(make([]byte, 0, len("event: error\ndata: ")+len(message)+48))
	buf.WriteString(`event: error` + "\ndata: ")
	buf.WriteString(`{"error":{"message":`)
	m, _ := json.Marshal(message)
	buf.Write(m)
	buf.WriteString(`,"type":"server_error"}}` + "\n\n")
	return buf.Bytes()
}

// streamErrMessage 提取错误帧的 message 文案：
func streamErrMessage(err error) string {
	type rawJSONer interface{ RawJSON() string }
	if rj, ok := err.(rawJSONer); ok {
		if raw := rj.RawJSON(); raw != "" {
			if s := upstreamErrMsg([]byte(raw)); s != "" {
				return domain.TruncateErrMsg(s)
			}
		}
	}
	return "upstream connection error"
}
