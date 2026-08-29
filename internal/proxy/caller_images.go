// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// imagesCaller 是 openai-images 格式的 UpstreamCaller（Task B 直连面）：
// 转发模板 base_url + /v1/images/<path>（generations|edits）。api_key /
// responses-special 两类型直连（responses-special 不因 resp 检测面类型分化
// 排除 images 直连；images 直连不经 strip_image_tools——images 请求无 tools
// 字段可剥，直连路径与 strip 面无关，spec §6 作用域声明）。codex 类型分流
// 到 codexImagesCaller（T2，见 caller_images_codex.go）。双协议：
//   - JSON：model 顶层提取 + setModel 模型映射改写（与 chat 同构）+ stream
//     探测（SSE 透传）
//   - multipart：body 原样透传（含图片文件字节与 boundary，Content-Type 保
//     留）；不做 setModel/setStreamAndModel JSON 重写（form model 原样透传）
type imagesCaller struct {
	p    *Proxy
	path string // 上游路径（"images/generations" | "images/edits"）
	op   domain.OperationTag
}

func (c *imagesCaller) operationTag() domain.OperationTag { return c.op }

func (c *imagesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	contentType := r.Header.Get("Content-Type")
	multipart := isMultipartForm(contentType)
	reqModel := gjson.GetBytes(body, "model").String()
	if multipart {
		reqModel = imagesMultipartModel(body, contentType)
	}
	upBody := body
	if !multipart {
		if nb, err := setModel(body, sel.Model); err == nil {
			upBody = nb
		}
	}
	upCT := ""
	if multipart {
		upCT = contentType
	}
	opTag := OperationTag(c.op)

	if stream {
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		observer := newImagesObserver(p, sel, reqModel, opTag)
		resp, err := p.clients.ImagesRaw(ctx, sel.TemplateID, sel.BaseURL, c.path, cred, upCT, upBody)
		if err != nil {
			code := statusOf(err)
			commit := CommitNotSent
			if code != 0 {
				commit = CommitUpstreamResponded
			}
			terminal := true
			if code == 429 || code == 0 {
				terminal = false
			}
			outcome := imagesOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), commit, false, terminal, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: err.Error()}
			_ = observer.Complete(outcome, health)
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			code := resp.StatusCode
			terminal := true
			if code == 429 {
				terminal = false
			}
			outcome := imagesOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), CommitUpstreamResponded, false, terminal, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: string(rb)}
			_ = observer.Complete(outcome, health)
			return resp.StatusCode, rb, false, nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var ttft *int64
		var imgCount, imgII, imgIO int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Observer: func(ev sserelay.Event) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				if ok, ii, io := billing.ImageStreamEvent(ev.Data); ok {
					imgCount++
					imgII, imgIO = ii, io
				}
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		u := usageTuple{ii: imgII, io: imgIO, tt: imgII + imgIO, calls: imgCount}
		usage := AttemptUsage{InputTokens: imgII, OutputTokens: imgIO, CallCount: imgCount}
		timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds(), TTFTMS: ttft}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				outcome := imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultClientCancel, 0, CommitResponseStarted, ttft != nil, true, false)
				_ = observer.Cancel(outcome)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusOK, domain.ErrAbort, u, start)))
				return 0, nil, true, nil
			}
			code := statusOf(err)
			commit := CommitUpstreamResponded
			business := false
			if code == 0 {
				commit = CommitSentAmbiguous
				business = true
			}
			outcome := imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultFailed, AttemptStatus(code), commit, business, true, false)
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: err.Error()}
			_ = observer.Complete(outcome, health)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
		}
		outcome := imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		_ = observer.Complete(outcome, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, 200, domain.ErrNone, u, start)))
		return 200, nil, true, nil
	}

	resp, err := p.clients.ImagesRaw(ctx, sel.TemplateID, sel.BaseURL, c.path, cred, upCT, upBody)
	if err != nil {
		observer := newImagesObserver(p, sel, reqModel, opTag)
		code := statusOf(err)
		commit := CommitNotSent
		if code != 0 {
			commit = CommitUpstreamResponded
		}
		terminal := true
		if code == 429 || code == 0 {
			terminal = false
		}
		outcome := imagesOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), commit, false, terminal, false)
		health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: err.Error()}
		_ = observer.Complete(outcome, health)
		return statusOf(err), upstreamBody(err), false, err
	}
	if resp.StatusCode != http.StatusOK {
		observer := newImagesObserver(p, sel, reqModel, opTag)
		rb := readUpstreamBody(resp)
		resp.Body.Close()
		code := resp.StatusCode
		terminal := true
		if code == 429 {
			terminal = false
		}
		outcome := imagesOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, AttemptStatus(code), CommitUpstreamResponded, false, terminal, false)
		health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: string(rb)}
		_ = observer.Complete(outcome, health)
		return resp.StatusCode, rb, false, nil
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		observer := newImagesObserver(p, sel, reqModel, opTag)
		outcome := imagesOutcome(reqID, sel, reqModel, opTag, AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}, AttemptUsage{}, ResultFailed, 0, CommitNotSent, false, false, false)
		health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(0), ErrorMessage: err.Error()}
		_ = observer.Complete(outcome, health)
		return 0, nil, false, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	ii, io, count := billing.ImageUsageFromResponse(data)
	observer := newImagesObserver(p, sel, reqModel, opTag)
	usage := AttemptUsage{InputTokens: ii, OutputTokens: io, CallCount: count}
	timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}
	outcome := imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	_ = observer.Complete(outcome, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, 200, domain.ErrNone, usageTuple{ii: ii, io: io, tt: ii + io, calls: count}, start)))
	return 200, nil, true, nil
}

func newImagesObserver(p *Proxy, sel *scheduler.Selection, reqModel string, op OperationTag) *AttemptObserver {
	mark := func(o AttemptOutcome, e AttemptHealthEvent) {
		p.sched.MarkResult(o.AccountID, e.Kind, e.ResetAt, int(o.HTTPStatus), e.ErrorMessage, o.MappedModel)
	}
	return NewAttemptObserver(nil, mark, nil, sel.Release)
}

func imagesOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = "fp-" + reqID
	}
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID("rc-" + reqID), QualityClassID: QualityClassID("qc-" + reqID), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerImages, OperationTag: op, Ordinal: 1, LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: commit, Result: result, HTTPStatus: status, Timing: timing, Usage: usage,
		BusinessFrameSent: businessSent, Terminal: terminal, IsMalformed: malformed,
	}
}

// isMultipartForm Content-Type 是否 multipart/form-data（Task B images 双协议
// 分支判定：multipart 走专用 body 处理——跳过 json.Valid 硬门与 gjson 顶层
// 提取）。
func isMultipartForm(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "multipart/form-data"
}

// imagesMultipartModel 从 multipart body 提取 model 字段值（P1-2：model 从
// form 字段取——不做 setModel JSON 重写，form model 原样透传）。body 已在
// 内存（MaxBytesReader 已限界），文件 part 不读内容（NextPart 仅解析 part
// 头，文件字节原样留在 body 透传）；model 字段值域内截断 4096（防恶意大
// 字段；form 文本字段正常值远小于此）。缺失/解析失败 → ""（调度器按格式
// 桶回退，与 JSON 缺 model 同语义）。
func imagesMultipartModel(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "model" {
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			return strings.TrimSpace(string(b))
		}
	}
}

// isCodexCredentialType codex 号池类型判定（Task B 分流骨架起：images 端点
// codex-oauth/codex-pat 模板选号命中 → codexImagesCaller（T2 落位；流式 T3））。
func isCodexCredentialType(t credential.Type) bool {
	return t == credential.TypeCodexOAuth || t == credential.TypeCodexPAT
}
