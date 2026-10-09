// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// imagesCaller 是 openai-images 格式的 UpstreamCaller：
// 转发模板 base_url + /v1/images/<path>（generations|edits）。api_key /
// responses-special 两类型直连；images 直连不经 strip_image_tools。codex 类型分流
// 到 codexImagesCaller。双协议：
//   - JSON：model 顶层提取 + setModel 模型映射改写 + stream 探测（SSE 透传）
//   - multipart：body 原样透传（含图片文件字节与 boundary，Content-Type 保留）；
//     不做 JSON 重写，form model 原样透传
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
	// multipart 原样透传，保留 boundary；JSON 才做模型映射改写。
	upBody := body
	if !multipart {
		nb, err := setModel(body, sel.Model)
		if err != nil {
			// 模型映射改写失败（body 非 JSON 对象等）：不得静默转发未映射 body，
			// 返回错误交 failover 循环分类（handled=false，对齐 caller_chat 的
			// setStreamAndModel 失败语义——S6 修复）。
			return 0, nil, false, err
		}
		upBody = nb
	}
	upCT := ""
	if multipart {
		upCT = contentType
	}
	opTag := OperationTag(c.op)
	// generations 与 edits 共用图像计费口径，但 operation tag 必须保持独立，避免质量数据碰撞。

	if stream {
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		resp, err := p.clients.ImagesRaw(ctx, sel.TemplateID, sel.BaseURL, c.path, cred, upCT, upBody, r.Header)
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
		// typed images（真上游 SSE 流）随五路传通用保活间隔（可静默 → 需通用
		// 保活）。仍经统一 Output 获得写前 seam 与出口判定。
		out := sserelay.NewOutput(w, p.cfg.StreamKeepaliveInterval, sserelay.OutputOptions{Ctx: ctx, Cancel: cancel})
		defer out.Release()
		// 首帧到达即记录 TTFT（写出前 seam）；每帧按 image 事件提取张数与 image tokens。
		var ttft *int64
		var imgCount, imgII, imgIO int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Output: out,
			OnEvent: func(ev sserelay.Event) {
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
			// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
			kind := classifyStreamExit(ctx, out, err, usage, ttft)
			if kind == streamExitClientCancel {
				// 图像流客户端断开不触发健康惩罚；保留已采 usage 记 200+ErrAbort。
				outcome := mergeDispatchBase(ctx, imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultClientCancel, 0, CommitResponseStarted, ttft != nil, true, false))
				p.observeDispatchOutcome(ctx, outcome, nil)
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIImages, http.StatusOK, domain.ErrAbort, u, start)))
				return 0, nil, true, nil
			}
			if kind == streamExitUncommitted {
				// 未提交 → pipeline（handled=false，可 failover/写 JSON）；缓冲残余丢弃。
				return statusOf(err), nil, false, err
			}
			if kind == streamExitCommitted {
				writeClientStreamError(out, err)
			}
			code := statusOf(err)
			commit := CommitUpstreamResponded
			business := false
			if code == 0 {
				commit = CommitSentAmbiguous
				business = true
			}
			outcome := mergeDispatchBase(ctx, imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultFailed, AttemptStatus(code), commit, business, true, false))
			health := &AttemptHealthEvent{Kind: scheduler.RuleKindOf(code), ErrorMessage: err.Error()}
			p.observeDispatchOutcome(ctx, outcome, health)
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIImages, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
		}
		outcome := mergeDispatchBase(ctx, imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false))
		health := &AttemptHealthEvent{Kind: rule.KindOK}
		p.observeDispatchOutcome(ctx, outcome, health)
		p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIImages, 200, domain.ErrNone, u, start)))
		return 200, nil, true, nil
	}

	resp, err := p.clients.ImagesRaw(ctx, sel.TemplateID, sel.BaseURL, c.path, cred, upCT, upBody, r.Header)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	if resp.StatusCode != http.StatusOK {
		rb := readUpstreamBody(resp)
		resp.Body.Close()
		// 非流式透传：归一 502 仅用于流式站点（非流式语义属 spec 非目标）。
		return resp.StatusCode, rb, false, nil
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, nil, false, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	// 非流式按响应 data 长度计张数，image tokens 单独计费。
	ii, io, count := billing.ImageUsageFromResponse(data)
	usage := AttemptUsage{InputTokens: ii, OutputTokens: io, CallCount: count}
	timing := AttemptTiming{LatencyMS: time.Since(start).Milliseconds()}
	outcome := mergeDispatchBase(ctx, imagesOutcome(reqID, sel, reqModel, opTag, timing, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false))
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	p.observeDispatchOutcome(ctx, outcome, health)
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIImages, 200, domain.ErrNone, usageTuple{ii: ii, io: io, tt: ii + io, calls: count}, start)))
	return 200, nil, true, nil
}

func imagesOutcome(reqID string, sel *scheduler.Selection, reqModel string, op OperationTag, timing AttemptTiming, usage AttemptUsage, result AttemptResult, status AttemptStatus, commit CommitState, businessSent, terminal, malformed bool) AttemptOutcome {
	return syntheticOutcome(CallerImages, reqID, sel, reqModel, op, timing, usage, result, status, commit, businessSent, terminal, malformed)
}

// isMultipartForm 判定是否为 multipart/form-data（images 双协议分支）。
func isMultipartForm(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "multipart/form-data"
}

// imagesMultipartModel 从 multipart body 提取 model 字段（form 原样透传，不做 JSON 重写）。
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
