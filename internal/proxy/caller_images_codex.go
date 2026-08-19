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
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
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

// errCodexImagesNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——main
// 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network）。
var errCodexImagesNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex image generation unavailable (adapter not wired)"}

// codexImagesCaller 是 codex-oauth/codex-pat 类型的 images 端点调用器（T2 §2，
// B 的 501 分流骨架落位）：网关解析请求体 → domain.ImageGenParams → 适配层
// GenerateImage（SDK 直连 codex images 端点，非流式）→ 响应统一走
// domain.ImageResponse 口径 → wire 序列化转发 + 计费提取（复用 C 的
// image_usage 提取纯函数——data 长 = 张数 + usage image_tokens → ImageCost，
// 与 api_key 直连同口径）。流式（T3）：GenerateImageStream 合成事件流 →
// streamImageGeneration（SSE 透传/keepalive/流终+abort 计费——T3 生产接线
// 点，同签名直赋适配层方法）。
// codexImagesCaller 无路径字段（评审 P3-1）：上游端点由 SDK 按参数派生
// （ImageGenParams.Images 非空 → edits，否则 generations）——与
// imagesCaller.path（直连面拼 URL）不同，codex 面端点选择归 SDK。
type codexImagesCaller struct {
	p *Proxy
}

func (c *codexImagesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	contentType := r.Header.Get("Content-Type")
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		// reqModel 冷路径直取（未装配 = 服务器配置错误罕见；成功热路径的第 8
		// 次全文档扫描已消除——见下方 params.Model 复用）。
		reqModel := gjson.GetBytes(body, "model").String()
		if isMultipartForm(contentType) {
			reqModel = imagesMultipartModel(body, contentType)
		}
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexImagesNotIntegrated.msg)
		writeErr(w, errCodexImagesNotIntegrated)
		return 0, nil, true, nil
	}
	params, err := imageParamsFromBody(body, contentType)
	if err != nil {
		// 本地参数拒绝（post-Select——Release + recordRejected + 400；评审
		// P2-1 语义：拒绝走 err_logs 审计）。模型映射改写对 codex 无字节透传
		// 约束，模型兜底走下方 sel.Model。reqModel 冷路径直取（同 501）。
		reqModel := gjson.GetBytes(body, "model").String()
		if isMultipartForm(contentType) {
			reqModel = imagesMultipartModel(body, contentType)
		}
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return 0, nil, true, nil
	}
	// 客户端请求模型（日志口径）：复用单遍解析结果 params.Model（JSON 顶层 /
	// multipart form model 同源）——A-P2-9 不再第 8 次 gjson 全文档扫描。
	reqModel := params.Model
	// 模型映射：sel.Model（调度器已应用 ModelMapping——与直连路径 setModel
	// 改写同语义；multipart 直连不做改写是字节透传约束，codex 网关重建 body
	// 无此约束）。缺模型（multipart 无 form model 等边角）→ 请求模型兜底。
	if params.Model == "" {
		params.Model = sel.Model
	}
	if params.Model == "" {
		params.Model = reqModel
	}
	// cred 派生（T1 已定义）：AccountExt → AccountCredential；BaseURL = 模板
	// base 派生完整 generations 端点（空 → SDK 内置 DefaultImagesURL）。
	cred2 := domain.CredentialFromExt(sel.Ext)
	baseURL, err := codexImagesBaseURL(sel.BaseURL)
	if err != nil {
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return 0, nil, true, nil
	}
	cred2.BaseURL = baseURL
	if stream {
		// 流式（T3 生产接线——同签名直赋适配层 GenerateImageStream）：参数/
		// 凭据派生与上共用，事件流 → streamImageGeneration（SSE 透传 + 首事件
		// 头 + keepalive ": ping" + completed 帧 + 流终/abort 计费，全在其内）。
		return p.streamImageGeneration(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred2, params, p.codex.GenerateImageStream)
	}
	// 非流式超时（B-P2-7，与 resp 非流式同款——images 非流式同病）：各自包 ctx
	// 超时（HTTPClient.Timeout 不可用：流式/非流式共享 client，覆盖整响应体读取
	// 会切断长流式 SSE）；黑洞读停滞不无限挂起，failover 可转移。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	img, err := p.codex.GenerateImage(ctx, &cred2, params)
	if err != nil {
		// 错误分类（骨架 statusOf/upstreamBody 零改动复用——信封协议）：
		//   - 信封（*HTTPError 包装——403 账号无生图权限等）→ 4xx 透传 /
		//     429/5xx failover 既有分类
		//   - fatal（errors.As 五类）→ 适配层已统一回调上报（账号失效标记 +
		//     FailAccount 快照摘除——failover 不重试同账号）；code 0 → 连接级
		//     MarkResult(RuleKindOf(0)) + 转移其它账号
		//   - RefreshError/网络 → code 0 → failover 可重试
		return statusOf(err), upstreamBody(err), false, err
	}
	// 成功：wire 序列化（客户端转发与计费提取共用同一字节——C 提取纯函数
	// ImageUsageFromResponse 与 API-key 直连同口径）。
	wire, err := sdkbridge.MarshalImageResponse(img)
	if err != nil {
		return 0, nil, false, err // 序列化失败（理论上不可达）→ 连接级 failover
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wire)
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	// 计费提取：data 长 = 张数 + usage image_tokens → usageTuple → finish 的
	// applyImageBilling（GetImagePrice → ImageCost，倍率整单施加）。
	ii, io, count := billing.ImageUsageFromResponse(wire)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusOK, domain.ErrNone, usageTuple{ii: ii, io: io, tt: ii + io, calls: count}, start)))
	return http.StatusOK, nil, true, nil
}

// codexImagesFor 按端点路径选 codex images 调用器（与 imagesCallerFor 同形态；
// New 构造的调用器复用，per-request 零分配）。
func (p *Proxy) codexImagesFor(r *http.Request) UpstreamCaller {
	if strings.HasSuffix(r.URL.Path, "/edits") {
		return p.codexImagesEdits
	}
	return p.codexImagesGenerations
}

// codexImagesBaseURL 模板 base → SDK WithBaseURL 覆盖值（**完整 generations
// 端点直用**——SDK 语义 client.go:137-144：覆盖值按完整端点直用，edits 由尾段
// generations→edits 派生，覆盖值必须传 generations 端点）。模板 base 空 → ""
// （SDK 内置 DefaultImagesURL）；解析失败 → 错误（配置错误本地拒绝，与直连
// 路径 parseFullURL 错误同语义）。
func codexImagesBaseURL(base string) (string, error) {
	if base == "" {
		return "", nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid template base URL %q: %w", base, err)
	}
	return u.JoinPath("images/generations").String(), nil
}

// imageParamsFromBody 请求体 → domain.ImageGenParams（T2 §2：网关解析传结构体
// ——SDK 不做 HTTP 协议解析）。JSON：顶层提取（model/prompt 必填；n/size/
// quality/background 可选；edits 输入 images:[{image_url}]）；multipart：form
// 字段（model/prompt/n/size/quality/background）+ 图片文件 part（FormName
// image 前缀 → Raw 字节，SDK 内部转 data URL）。
func imageParamsFromBody(body []byte, contentType string) (*domain.ImageGenParams, error) {
	if isMultipartForm(contentType) {
		return imageParamsMultipart(body, contentType)
	}
	return imageParamsJSON(body)
}

// nullLit JSON null 字面量（可选字段 null → 按缺省忽略——gjson Type 判定同
// 语义；encoding/json 对 null 解到非指针值为 no-op 不报错，需显式区分）。
var nullLit = []byte("null")

func imageParamsJSON(body []byte) (*domain.ImageGenParams, error) {
	if !json.Valid(body) { // 防御：handleFormat 已过 json.Valid 硬门
		return nil, errors.New("invalid request body: invalid JSON")
	}
	// 单遍解析（A-P2-9：原 7 次 gjson.GetBytes 各从头单遍扫描 + Call 侧第 8 次
	// → json.Unmarshal 单遍——MB 级 base64 data URL body 每请求 ~8 遍全文档扫
	// 描 → 1 遍）。可选字段走 json.RawMessage（body 子切片零拷贝）——宽松语义
	// 与 gjson 缺字段默认一致：缺失 → nil，类型不合（字符串 n / 数字 size /
	// null 等）→ 按缺省忽略（gjson Type 判定同语义）。
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
		if err := json.Unmarshal(raw.N, &f); err == nil { // 数字才认（整数/小数截断——gjson Int 语义）；字符串 → 忽略
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
	// edits 输入图（JSON 形态 images:[{image_url}]，官方文档实证）；file_id
	// 形态不映射（需文件上传面，SDK 无此能力——忽略）。元素非对象 / image_url
	// 缺失或非字符串 → 跳过（gjson Get("image_url").String() 同语义）。
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
			// 图片文件 part（image / image[] 等 FormName）→ Raw 字节（body 已
			// 在内存 MaxBytesReader 限界，SDK 内部转 data URL）；其余字段忽略。
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
