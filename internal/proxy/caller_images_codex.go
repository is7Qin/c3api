// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// errCodexImagesNotIntegrated 501：codex 适配层未装配。
var errCodexImagesNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex image generation unavailable (adapter not wired)"}

// codexImagesCaller 是 codex 类型的 images 端点调用器：网关解析请求体 →
// domain.ImageGenParams → 适配层 GenerateImage → 统一 wire 序列化转发 + 计费
// 提取（按张数与 image tokens），流式走 streamImageGeneration。
// codex 面固定官方端点 generations/edits，选择由 SDK 决定。
type codexImagesCaller struct {
	p *Proxy
}

func (c *codexImagesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	contentType := r.Header.Get("Content-Type")
	if p.codex == nil {
		reqModel := gjson.GetBytes(body, "model").String()
		if isMultipartForm(contentType) {
			reqModel = imagesMultipartModel(body, contentType)
		}
		sel.Release()
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexImagesNotIntegrated.msg)
		writeErr(w, errCodexImagesNotIntegrated)
		return 0, nil, true, nil
	}
	params, err := imageParamsFromBody(body, contentType)
	if err != nil {
		reqModel := gjson.GetBytes(body, "model").String()
		if isMultipartForm(contentType) {
			reqModel = imagesMultipartModel(body, contentType)
		}
		sel.Release()
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return 0, nil, true, nil
	}
	reqModel := params.Model
	if params.Model == "" {
		params.Model = sel.Model
	}
	if params.Model == "" {
		params.Model = reqModel
	}
	cred2 := domain.CredentialFromExt(sel.Ext)
	if stream {
		return p.streamImageGeneration(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred2, params, p.codex.GenerateImageStream)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	opTag := codexImagesOpTag(r)
	img, err := p.codex.GenerateImage(ctx, &cred2, params)
	if err != nil {
		if sdkbridge.IsFatal(err) {
			code := statusOf(err)
			if code == 0 {
				code = http.StatusBadGateway
			}
			return code, upstreamBody(err), false, err
		}
		return statusOf(err), upstreamBody(err), false, err
	}
	wire, err := sdkbridge.MarshalImageResponse(img)
	if err != nil {
		return 0, nil, false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wire)
	// 计费按响应张数与 image tokens 落账，复用直连面同口径提取。
	ii, io, count := billing.ImageUsageFromResponse(wire)
	usage := AttemptUsage{InputTokens: ii, OutputTokens: io, CallCount: count}
	timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}
	observer := newCodexImagesObserver(p, sel, reqModel, opTag)
	outcome := codexImagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	_ = observer.Complete(outcome, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusOK, domain.ErrNone, usageTuple{ii: ii, io: io, tt: ii + io, calls: count}, start)))
	return http.StatusOK, nil, true, nil
}

func codexImagesOpTag(r *http.Request) OperationTag {
	if strings.HasSuffix(r.URL.Path, "/edits") {
		return OperationTag(domain.OpImagesEdits)
	}
	return OperationTag(domain.OpImagesGenerations)
}

func newCodexImagesObserver(p *Proxy, sel *scheduler.Selection, reqModel string, op OperationTag) *AttemptObserver {
	mark := func(o AttemptOutcome, e AttemptHealthEvent) {
		p.sched.MarkResult(o.AccountID, e.Kind, e.ResetAt, int(o.HTTPStatus), e.ErrorMessage, o.MappedModel)
	}
	return NewAttemptObserver(nil, mark, nil, sel.Release)
}

func codexImagesOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = "fp-" + reqID
	}
	return AttemptOutcome{
		ID: AttemptID(reqID), RouteClassID: RouteClassID("rc-" + reqID), QualityClassID: QualityClassID("qc-" + reqID), Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: reqModel, MappedModel: sel.Model,
		CallerCategory: CallerImagesCodex, OperationTag: op, Ordinal: 1, LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: commit, Result: result, HTTPStatus: status, Timing: timing, Usage: usage,
		BusinessFrameSent: businessSent, Terminal: terminal, IsMalformed: malformed,
	}
}

func isCodexFatal(err error) bool {
	return sdkbridge.IsFatal(err)
}

// codexImagesFor 按端点路径选择 codex 调用器。
func (p *Proxy) codexImagesFor(r *http.Request) UpstreamCaller {
	if strings.HasSuffix(r.URL.Path, "/edits") {
		return p.codexImagesEdits
	}
	return p.codexImagesGenerations
}

// imageParamsFromBody 将请求体解析为 ImageGenParams（JSON 与 multipart 双协议）。
func imageParamsFromBody(body []byte, contentType string) (*domain.ImageGenParams, error) {
	if isMultipartForm(contentType) {
		return imageParamsMultipart(body, contentType)
	}
	return imageParamsJSON(body)
}

// nullLit JSON null 字面量，用于区分缺省与显式 null。
var nullLit = []byte("null")

func imageParamsJSON(body []byte) (*domain.ImageGenParams, error) {
	if !json.Valid(body) {
		return nil, errors.New("invalid request body: invalid JSON")
	}
	var raw struct {
		Model      string            `json:"model"`
		Prompt     string            `json:"prompt"`
		N          json.RawMessage   `json:"n"`
		Size       json.RawMessage   `json:"size"`
		Quality    json.RawMessage   `json:"quality"`
		Background json.RawMessage   `json:"background"`
		Images     []json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("invalid request body: invalid JSON")
	}
	if raw.Model == "" {
		return nil, errors.New("invalid request body: model required")
	}
	if raw.Prompt == "" {
		return nil, errors.New("invalid request body: prompt required")
	}
	p := &domain.ImageGenParams{Model: raw.Model, Prompt: raw.Prompt}
	if len(raw.N) > 0 && !bytes.Equal(raw.N, nullLit) {
		var f float64
		if err := json.Unmarshal(raw.N, &f); err == nil {
			n := int(f)
			p.N = &n
		}
	}
	if len(raw.Size) > 0 && !bytes.Equal(raw.Size, nullLit) {
		var s string
		if json.Unmarshal(raw.Size, &s) == nil {
			p.Size = &s
		}
	}
	if len(raw.Quality) > 0 && !bytes.Equal(raw.Quality, nullLit) {
		var s string
		if json.Unmarshal(raw.Quality, &s) == nil {
			p.Quality = &s
		}
	}
	if len(raw.Background) > 0 && !bytes.Equal(raw.Background, nullLit) {
		var s string
		if json.Unmarshal(raw.Background, &s) == nil {
			p.Background = &s
		}
	}
	for _, ir := range raw.Images {
		var e struct {
			ImageURL string `json:"image_url"`
		}
		if json.Unmarshal(ir, &e) != nil || e.ImageURL == "" {
			continue
		}
		uu := e.ImageURL
		p.Images = append(p.Images, domain.ImageRef{ImageURL: &uu})
	}
	return p, nil
}

func imageParamsMultipart(body []byte, contentType string) (*domain.ImageGenParams, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, errors.New("invalid request body: multipart content type parse failed")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	p := &domain.ImageGenParams{}
	var images []domain.ImageRef
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid request body: multipart parse failed")
		}
		switch part.FormName() {
		case "model":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			p.Model = strings.TrimSpace(string(b))
		case "prompt":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			p.Prompt = strings.TrimSpace(string(b))
		case "n":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				p.N = &v
			}
		case "size":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			s := strings.TrimSpace(string(b))
			if s != "" {
				p.Size = &s
			}
		case "quality":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			s := strings.TrimSpace(string(b))
			if s != "" {
				p.Quality = &s
			}
		case "background":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			s := strings.TrimSpace(string(b))
			if s != "" {
				p.Background = &s
			}
		default:
			if strings.HasPrefix(part.FormName(), "image") {
				b, err := io.ReadAll(part)
				if err != nil {
					return nil, errors.New("invalid request body: image part read failed")
				}
				images = append(images, domain.ImageRef{Raw: b})
			}
		}
	}
	if p.Model == "" {
		return nil, errors.New("invalid request body: model required")
	}
	if p.Prompt == "" {
		return nil, errors.New("invalid request body: prompt required")
	}
	p.Images = images
	return p, nil
}
