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
}

func (c *imagesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	contentType := r.Header.Get("Content-Type")
	multipart := isMultipartForm(contentType)
	// 客户端请求模型：JSON 形态 gjson 顶层提取；multipart 形态 form 字段提取
	// （handleFormat 已提取一次用于调度——调用器侧再取一次供日志，与 chat
	// caller 的 reqModel 提取同构；body 已在内存，零 IO）。
	reqModel := gjson.GetBytes(body, "model").String()
	if multipart {
		reqModel = imagesMultipartModel(body, contentType)
	}
	// 模型映射改写：JSON 形态 setModel（ModelMapping 语义，与 chat 同构；
	// 改写失败原样转发——body 已过 json.Valid，防御性兜底）；multipart 形态
	// 不做改写（form model 字段原样透传，spec §5.1 声明）。
	upBody := body
	if !multipart {
		if nb, err := setModel(body, sel.Model); err == nil {
			upBody = nb
		}
	}
	// 上游 Content-Type：multipart 保留客户端完整值（boundary 必须一致——
	// 转发字节原样）；JSON 空串由 aiclient 补 application/json。
	upCT := ""
	if multipart {
		upCT = contentType
	}

	if stream {
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		resp, err := p.clients.ImagesRaw(ctx, sel.TemplateID, sel.BaseURL, c.path, cred, upCT, upBody)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			return resp.StatusCode, rb, false, nil
		}
		// SSE 响应头与 chat 流式一致（relay 只转发字节，不代设头）
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		// TTFT 采集（首 chunk 时间毫秒，同 chat 流式）+ usage 提取（A-P1-2 接
		// 线——每帧 data 走 billing.ImageStreamEvent：count 累积 + ii/io 取最后
		// 一个 completed 帧的 usage（覆盖语义，对齐 caller_images_stream.go codex
		// 路径口径——写成累积求和多图请求差 N 倍）；即时提取标量、不保留跨帧
		// 切片（relay 缓冲复用纪律，relay.go:21-27）。
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
					imgII, imgIO = ii, io // 覆盖语义：usage 取末次 completed 帧
				}
			},
		})
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		// 流终落账元组（对齐 caller_images_stream.go 口径）：ii/io = image
		// tokens、tt = 之和、img = completed 帧计数；三处落账点共用。
		u := usageTuple{ii: imgII, io: imgIO, tt: imgII + imgIO, calls: imgCount}
		if err != nil {
			// 客户端断开：上游已消费请求（成功），仍须记录用量（同 chat 语义）。
			// errors.Is(err, context.Canceled) 即客户端断开——sserelay.normalize
			// 已区分三类（C-P2-2）：上游停滞超时 → DeadlineExceeded 走上游错误分支。
			if errors.Is(err, context.Canceled) {
				p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusOK, domain.ErrAbort, u, start)))
				return 0, nil, true, nil
			}
			p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, u, err)
			p.sched.MarkResult(sel.AccountID, scheduler.RuleKindOf(statusOf(err)), nil, statusOf(err), err.Error())
			return 0, nil, true, nil
		}
		p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "")
		p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, 200, domain.ErrNone, u, start)))
		return 200, nil, true, nil
	}

	// 非流式：直连透传——上游响应原样转发（响应零改写零损失）+ **计费提取
	// （T2 P3-4 遗留接入）**：复用 C 的 image_usage 提取纯函数
	// （ImageUsageFromResponse——data 长 = 张数 + usage image_tokens，与 codex
	// 路径同口径）→ ImageCost 落账含 image 分量（压测期直连计费分量恒 0 的
	// 收敛）。提取在已读入的转发字节上执行——零额外解析零分配。
	resp, err := p.clients.ImagesRaw(ctx, sel.TemplateID, sel.BaseURL, c.path, cred, upCT, upBody)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	if resp.StatusCode != http.StatusOK {
		rb := readUpstreamBody(resp)
		resp.Body.Close()
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
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "")
	// usage 提取（codexImagesCaller 同款形态：ii/io = image tokens，tt = 之和，
	// img = data 数组长；gjson 输入字节直读零分配）。
	ii, io, count := billing.ImageUsageFromResponse(data)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, 200, domain.ErrNone, usageTuple{ii: ii, io: io, tt: ii + io, calls: count}, start)))
	return 200, nil, true, nil
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
