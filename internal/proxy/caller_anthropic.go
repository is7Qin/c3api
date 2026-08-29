// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/sserelay"
)

type anthropicCaller struct{ p *Proxy }

func (c *anthropicCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	if stream {
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
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
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Observer: func(ev sserelay.Event) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				switch string(ev.EventName()) {
				case "message_start":
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
		base := anthropicBaseOutcome(reqID, groupID, sel, reqModel, start, ttft, it, ot, cr, cc)
		obs := anthropicObserver(p, sel)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				out := base
				out.Result = ResultClientCancel
				out.Commit = CommitResponseStarted
				out.HTTPStatus = 0
				out.Terminal = true
				out.BusinessFrameSent = true
				_ = obs.Cancel(out)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
				return 0, nil, true, nil
			}
			out := base
			out.Result = ResultFailed
			out.HTTPStatus = 0
			out.Commit = CommitSentAmbiguous
			out.Terminal = true
			out.BusinessFrameSent = true
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(statusOf(err)), ErrorMessage: err.Error()}
			_ = obs.Complete(out, health)
			if p.log != nil {
				p.log.Warn("upstream stream aborted", logx.String("request_id", reqID))
			}
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: it + ot, cr: cr, cc: cc}, start)))
			return 0, nil, true, nil
		}
		tt = it + ot
		base.Usage.OutputTokens = ot
		base.Usage.InputTokens = it
		base.Timing.TTFTMS = ttft
		base.Timing.LatencyMS = time.Since(start).Milliseconds()
		out := base
		out.Result = ResultSuccess
		out.HTTPStatus = 200
		out.Commit = CommitClientCommitted
		out.Terminal = true
		out.BusinessFrameSent = true
		out.Usage = AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		_ = obs.Complete(out, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
		return 200, nil, true, nil
	}
	var params anthropic.MessageNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		sel.Release()
		return 400, nil, true, nil
	}
	reqModel := params.Model
	params.Model = sel.Model
	tpl := tplOf(sel)
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
		it, ot, tt, cr, cc = anthropicUsageFromResponse(resp.Usage)
	}
	base := anthropicBaseOutcome(reqID, groupID, sel, reqModel, start, nil, it, ot, cr, cc)
	obs := anthropicObserver(p, sel)
	out := base
	out.Result = ResultSuccess
	out.HTTPStatus = 200
	out.Commit = CommitClientCommitted
	out.Terminal = true
	out.BusinessFrameSent = true
	out.Usage = AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc}
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	_ = obs.Complete(out, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatAnthropic, 200, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}, start)))
	return 200, nil, true, nil
}

func anthropicBaseOutcome(reqID string, groupID int64, sel *scheduler.Selection, reqModel string, start time.Time, ttft *int64, it, ot, cr, cc int64) AttemptOutcome {
	routeID, _ := domain.RouteClassID(groupID, domain.FormatAnthropic, reqModel, domain.OpAnthropicMessages)
	qualityID, _ := domain.QualityClassID(domain.CallerAnthropic, domain.FormatAnthropic, sel.Model, domain.OpAnthropicMessages)
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = hex.EncodeToString(routeID[:8])
	}
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID(hex.EncodeToString(routeID[:])), QualityClassID: QualityClassID(hex.EncodeToString(qualityID[:])), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerAnthropic, OperationTag: OperationTag(domain.OpAnthropicMessages),
		Ordinal: 1, Lane: LanePrimary, Generation: 1, LifecycleRevision: 1,
		Timing: AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft},
		Usage: AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc},
	}
}

func anthropicObserver(p *Proxy, sel *scheduler.Selection) *AttemptObserver {
	return NewAttemptObserver(nil,
		func(o AttemptOutcome, e AttemptHealthEvent) { p.sched.MarkResult(o.AccountID, e.Kind, nil, int(o.HTTPStatus), e.ErrorMessage, o.MappedModel) },
		func(o AttemptOutcome) {},
		func() { sel.Release() },
	)
}
