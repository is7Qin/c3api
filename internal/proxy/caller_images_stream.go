// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/logx"
)

// imageStreamGenerator 流式生图能力，与 sdkbridge.Codex.GenerateImageStream 同签名。
type imageStreamGenerator func(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error

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
	var (
		count       int64
		usage       *domain.ImageUsage
		headersSent bool
		ttft        *int64
	)
	writeFrame := func(frame []byte) error {
		if !headersSent {
			headersSent = true
			writeSSEHeaders(w)
			flushWriter(w)
			if ttft == nil {
				ms := time.Since(start).Milliseconds()
				ttft = &ms
			}
		} else if ttft == nil {
			ms := time.Since(start).Milliseconds()
			ttft = &ms
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		flushWriter(w)
		return nil
	}

	genErr := gen(ctx, cred, params, func(ev domain.ImageStreamEvent) error {
		switch ev.Type {
		case domain.ImageStreamEventKeepalive:
			return writeFrame([]byte(": ping\n\n"))
		case domain.ImageStreamEventCompleted:
			count++
			if ev.Usage != nil {
				usage = ev.Usage
			}
			return writeFrame(buildCompletedFrame(&ev))
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

	if genErr != nil {
		if !headersSent {
			if sdkbridge.IsFatal(genErr) {
				code := statusOf(genErr)
				if code == 0 {
					code = http.StatusBadGateway
				}
				return code, upstreamBody(genErr), false, genErr
			}
			return statusOf(genErr), upstreamBody(genErr), false, genErr
		}
		// 响应头已发出后失败只能写 SSE error 帧；客户端断开与上游错误分开处理。
		_, _ = w.Write(buildErrorFrame(streamErrMessage(genErr)))
		flushWriter(w)
		if r.Context().Err() != nil {
			outcome := mergeDispatchBase(ctx, imagesStreamOutcome(reqID, sel, reqModel, opTag, timing, usageObs, ResultClientCancel, 0, CommitResponseStarted, headersSent, true, false))
			p.observeDispatchOutcome(ctx, outcome, nil)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), sel.Format, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
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

	if !headersSent {
		writeSSEHeaders(w)
		flushWriter(w)
		if ttft == nil {
			ms := time.Since(start).Milliseconds()
			ttft = &ms
			timing.TTFTMS = ttft
		}
	}
	outcome := mergeDispatchBase(ctx, imagesStreamOutcome(reqID, sel, reqModel, opTag, timing, usageObs, ResultSuccess, 200, CommitResponseStarted, true, true, false))
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	p.observeDispatchOutcome(ctx, outcome, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), sel.Format, http.StatusOK, domain.ErrNone, u, start)))
	return http.StatusOK, nil, true, nil
}

func imagesStreamOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = "fp-" + reqID
	}
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID("rc-" + reqID), QualityClassID: QualityClassID("qc-" + reqID), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerImagesCodex, OperationTag: op, Ordinal: 1, IdentityRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: commit, Result: result, HTTPStatus: status, Timing: timing, Usage: usage,
		BusinessFrameSent: businessSent, Terminal: terminal, IsMalformed: malformed,
	}
}

// writeSSEHeaders 发 SSE 响应头（text/event-stream）。
func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// flushWriter 逐事件 Flush。
func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
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

// buildErrorFrame 生成失败 SSE error 帧：
func buildErrorFrame(message string) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len("event: error\ndata: ")+len(message)+32))
	buf.WriteString("event: error\ndata: ")
	m, _ := json.Marshal(map[string]string{"message": message})
	buf.Write(m)
	buf.WriteString("\n\n")
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
