// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// AccountProber 是单账号单模型探测执行面（internal/probe.Prober 实现；接口定义
// 在 service 侧，main 构造注入）。nil = 未装配 → ProbeAccount 返回装配错误，
// handler 按默认 500 映射（装配错误必须显形，不降级为 503）。
type AccountProber interface {
	Probe(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult
}

// AccountProbeRequest 是已解码的探测请求（协议无关：handler 从 openapi 生成
// 类型搬运，service 只认 domain.RequestFormat）。Input/ImageMode/ImageURL 为
// 三态投影：nil = 缺省或显式 null（同义）；非 nil = 显式值（**含空串**，必须
// 与缺省区分：空串有各自的 400 规则与默认值）。
type AccountProbeRequest struct {
	Model     string
	Format    domain.RequestFormat
	Input     *string
	ImageMode *string
	ImageURL  *string
}

// probeFormats 可探测格式集合（七格式中的六个：openai-images 按 image_mode
// 分 generations/edits，openai-search 打 /v1/alpha/search）。
var probeFormats = map[domain.RequestFormat]struct{}{
	domain.FormatOpenAIChat:        {},
	domain.FormatOpenAIResponses:   {},
	domain.FormatOpenAIResponsesWS: {},
	domain.FormatAnthropic:         {},
	domain.FormatOpenAIImages:      {},
	domain.FormatOpenAISearch:      {},
}

// ProbeAccount 组装并执行一次单账号单模型探测（spec §5.3）。
//
// 只读：不落库、不计费、不改账号状态、不触发调度/规则/失败统计。请求体只能
// 指定 model/format——出站目标（base_url + 路径）完全来自账号/模板存储配置。
//
// 错误分流：
//   - 404：账号不存在/越域（仓储 scope）；
//   - 400：format/model 不合法、账号出口绑定不可用、缺少静态 key/codex 凭据；
//   - 500：仓储故障、探测组件未装配；
//   - 200 + success=false：探测结论（含 oauth 已过期——不发网络请求）。
func (s *Service) ProbeAccount(ctx context.Context, id int64, req AccountProbeRequest) (*domain.ProbeResult, error) {
	if s.prober == nil {
		return nil, errors.New("service: account probe prober not configured")
	}
	// 先做**不依赖存储**的输入校验：非法 format/超长 model 不得进入仓储等待
	// （否则大量并发请求会在 DB 池前排同样长的队并保留大字符串），错误文本也
	// 不回显任意长度的输入。
	format := req.Format
	model := req.Model
	if _, ok := probeFormats[format]; !ok {
		return nil, fmt.Errorf("%w: invalid probe format", ErrInvalidInput)
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: probe model is empty", ErrInvalidInput)
	}
	if len(model) > domain.ProbeMaxModelLen {
		return nil, fmt.Errorf("%w: probe model exceeds %d bytes", ErrInvalidInput, domain.ProbeMaxModelLen)
	}
	// image mode / image_url / input 的三态校验同样在**任何存储读取与准入之前**
	// 完成：非法输入只回固定文案（不回显 URL/原文），也不占探测槽位。
	imageMode, imageURL, verr := validateProbeImageFields(format, req.ImageMode, req.ImageURL)
	if verr != nil {
		return nil, verr
	}
	input, verr := probeInput(format, imageMode, req.Input)
	if verr != nil {
		return nil, verr
	}

	// 30s 总期限从输入校验后开始（含准入等待+DB+上游）；准入在仓储读取前
	// 限制 4 执行 + 16 等待，队列满立即 probe busy，避免无限请求在 DB 池前排队。
	pctx, cancel := context.WithTimeout(ctx, domain.ProbeTimeout)
	defer cancel()
	ctxResult := func(err error) *domain.ProbeResult {
		res := &domain.ProbeResult{Format: format, RequestedModel: model}
		if errors.Is(err, errProbeBusy) {
			res.Error = domain.ProbeErrorBusy
		} else {
			res.Error = domain.ProbeContextError(err)
		}
		s.logProbe(id, *res)
		return res
	}
	release, aerr := s.probeGate.acquire(pctx)
	if aerr != nil {
		return ctxResult(aerr), nil
	}
	defer release()

	acc, err := s.store.GetAccountWithTemplate(pctx, id)
	// 只要 store 已返回，先看期限：非 ctx 错误（404/500）也不能掩盖已过期。
	if pctx.Err() != nil {
		return ctxResult(pctx.Err()), nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ctxResult(err), nil
		}
		return nil, mapRepoErr(err)
	}
	// GET 单个账号允许读到软删行（编辑回显），探测是消费动作：已删除账号不得
	// 继续用其存储凭据发上游请求。错误文本与不存在保持同一形态，不暴露删除态。
	if acc.DeletedAt != nil {
		return nil, mapRepoErr(fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id))
	}
	tpl := acc.Template
	if tpl == nil {
		return nil, fmt.Errorf("service: account %d has no template", id)
	}
	if !tpl.FormatSupports(format, model) || !(tpl.Serves(model) || !tpl.HasModelSpace()) {
		return nil, fmt.Errorf("%w: model %q is not eligible for template format %q", ErrInvalidInput, model, format)
	}
	if err := pctx.Err(); err != nil {
		return ctxResult(err), nil
	}

	upstreamModel := domain.MapModel(tpl, model)
	baseURL := tpl.BaseURL
	if acc.BaseURL != nil && *acc.BaseURL != "" {
		baseURL = *acc.BaseURL
	}

	// 模板扩展只有 custom 的 upstream_paths 与探测相关；account 读取不 JOIN ext，
	// 因此单独读一次。ext 行缺失（repository.ErrNotFound）= 未配置（回落默认
	// 路径），其他错误才 500。
	var upstreamPaths map[string]string
	if tpl.CredentialType == credential.TypeCustom {
		ext, xerr := s.store.GetTemplateExt(pctx, tpl.ID)
		// ext 的 ErrNotFound/其它错误同样不能掩盖已过期期限。
		if pctx.Err() != nil {
			return ctxResult(pctx.Err()), nil
		}
		switch {
		case xerr == nil:
			if ext != nil {
				upstreamPaths = ext.UpstreamPaths
			}
		case errors.Is(xerr, repository.ErrNotFound):
			// 无 ext 行 = 未配置。
		case errors.Is(xerr, context.Canceled) || errors.Is(xerr, context.DeadlineExceeded):
			return ctxResult(xerr), nil
		default:
			return nil, xerr
		}
	}

	// 出口 fail-closed：绑定存在但不可用（行缺失/不可见/禁用）⇒ 400 且不发任何
	// 上游请求，绝不回落直连。
	var egressSpec egress.Spec
	if acc.ProxyID != nil {
		if acc.Proxy == nil || !acc.Proxy.Enabled {
			return nil, fmt.Errorf("%w: 账号出口绑定不可用", ErrInvalidInput)
		}
		egressSpec = acc.Proxy.EgressSpec()
	}
	if err := pctx.Err(); err != nil {
		return ctxResult(err), nil
	}

	var (
		token          string
		codexAccountID string
	)
	if tpl.CredentialType.IsCodex() {
		cred, cerr := s.store.GetOwnedAccountCredential(pctx, id)
		// 凭据的 ErrNotFound/其它错误也不能掩盖已过期期限。
		if pctx.Err() != nil {
			return ctxResult(pctx.Err()), nil
		}
		if cerr != nil {
			if errors.Is(cerr, repository.ErrNotFound) {
				return nil, fmt.Errorf("%w: 账号缺少 codex 凭据", ErrInvalidInput)
			}
			if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
				return ctxResult(cerr), nil
			}
			return nil, cerr
		}
		switch tpl.CredentialType {
		case credential.TypeCodexOAuth:
			if cred.OAuthToken == "" {
				return nil, fmt.Errorf("%w: 账号缺少 codex 凭据", ErrInvalidInput)
			}
			// 静态探测不能刷新：过期直接给出结论（刷新会轮换 RT 且本切片不落库
			// ⇒ 会毁掉账号），绝不发网络请求。期限优先：超时不得被早退文案掩盖。
			if pctx.Err() != nil {
				return ctxResult(pctx.Err()), nil
			}
			if cred.OAuthExpiresAt != nil && !cred.OAuthExpiresAt.After(time.Now()) {
				res := domain.ProbeResult{
					Format:         format,
					RequestedModel: model,
					UpstreamModel:  upstreamModel,
					Error:          domain.ProbeErrorCodexTokenExpired,
				}
				s.logProbe(id, res)
				return &res, nil
			}
			token = cred.OAuthToken
		case credential.TypeCodexPAT:
			if cred.PATKey == "" {
				return nil, fmt.Errorf("%w: 账号缺少 codex 凭据", ErrInvalidInput)
			}
			token = cred.PATKey
		default:
			return nil, fmt.Errorf("%w: unsupported credential type for probe", ErrInvalidInput)
		}
		codexAccountID = cred.CodexAccountID
	} else {
		if tpl.CredentialType != credential.TypeAPIKey &&
			tpl.CredentialType != credential.TypeResponsesSpecial &&
			tpl.CredentialType != credential.TypeCustom {
			return nil, fmt.Errorf("%w: unsupported credential type for probe", ErrInvalidInput)
		}
		if acc.UpstreamKey == "" {
			return nil, fmt.Errorf("%w: 账号缺少静态上游 key", ErrInvalidInput)
		}
		token = acc.UpstreamKey
	}

	spec := domain.ProbeSpec{
		AccountID:      acc.ID,
		TemplateID:     tpl.ID,
		CredentialType: tpl.CredentialType,
		Format:         format,
		RequestedModel: model,
		UpstreamModel:  upstreamModel,
		BaseURL:        baseURL,
		UpstreamPaths:  upstreamPaths,
		Credential:     token,
		CodexAccountID: codexAccountID,
		Egress:         egressSpec,
		Input:          input,
		ImageMode:      imageMode,
		ImageURL:       imageURL,
	}
	if err := pctx.Err(); err != nil {
		return ctxResult(err), nil
	}
	res := s.prober.Probe(pctx, spec)
	s.logProbe(id, res)
	return &res, nil
}

// logProbe 每个探测恰好一条 Info（§7）：不含凭据、不含响应体。失败另记固定
// 分类 error（prober 已按白名单/有限错误码收敛并截断）。
func (s *Service) logProbe(accountID int64, res domain.ProbeResult) {
	if s.log == nil {
		return
	}
	fields := []logx.Field{
		logx.Int64("account_id", accountID),
		logx.String("format", string(res.Format)),
		logx.String("requested_model", res.RequestedModel),
		logx.String("upstream_model", res.UpstreamModel),
		logx.Int("status_code", res.StatusCode),
		logx.Int64("latency_ms", res.LatencyMS),
		logx.Bool("success", res.Success),
	}
	if !res.Success && res.Error != "" {
		fields = append(fields, logx.String("error", res.Error))
	}
	s.log.Info("account_probe", fields...)
}

// validateProbeImageFields 是 image_mode/image_url 的**唯一**三态校验实现
// （spec §4「可选字段语义（唯一规则）」）：全部在 store/准入之前完成，错误
// 文案固定且不回显 image_url/原始值。
//
//   - 非 openai-images：出现任何非 null image_mode → probe image mode
//     unexpected；出现任何非 null image_url（含空串）→ probe image url invalid。
//   - images：image_mode 缺省/null → generations；空串/非法值 → probe image
//     mode invalid。
//   - edits：image_url 缺省/null/空串 → probe image url required；scheme 非
//     http(s)/data:image 或数据 URL 形态不完整 → probe image url invalid。
//   - generations：非空 image_url → probe image url invalid（空串可用）。
//   - 任何非 null image_url 超过 6 MiB → probe image url exceeds 6 MiB。
func validateProbeImageFields(format domain.RequestFormat, rawMode, rawURL *string) (string, string, error) {
	invalidInput := func(msg string) (string, string, error) {
		return "", "", fmt.Errorf("%w: %s", ErrInvalidInput, msg)
	}
	if format != domain.FormatOpenAIImages {
		if rawMode != nil {
			return invalidInput(domain.ProbeErrorImageModeUnexpected)
		}
		if rawURL != nil {
			return invalidInput(domain.ProbeErrorImageURLInvalid)
		}
		return "", "", nil
	}

	mode := domain.ProbeImageModeGenerations
	if rawMode != nil {
		switch *rawMode {
		case domain.ProbeImageModeGenerations:
			mode = domain.ProbeImageModeGenerations
		case domain.ProbeImageModeEdits:
			mode = domain.ProbeImageModeEdits
		default:
			return invalidInput(domain.ProbeErrorImageModeInvalid)
		}
	}
	if rawURL != nil && len(*rawURL) > domain.ProbeMaxImageSourceBytes {
		return invalidInput(domain.ProbeErrorImageURLTooLarge)
	}

	switch mode {
	case domain.ProbeImageModeEdits:
		if rawURL == nil || *rawURL == "" {
			return invalidInput(domain.ProbeErrorImageURLRequired)
		}
		if !validProbeImageURL(*rawURL) {
			return invalidInput(domain.ProbeErrorImageURLInvalid)
		}
		return mode, *rawURL, nil
	default:
		if rawURL != nil && *rawURL != "" {
			return invalidInput(domain.ProbeErrorImageURLInvalid)
		}
		return mode, "", nil
	}
}

// validProbeImageURL 校验 image_url 的 wire 形态：http(s) 绝对 URL，或
// data:image/<mime>;base64,<data>（scheme 大小写不敏感）。不做取图/解码
// （prober 负责；codex 族直接交给 SDK）。
func validProbeImageURL(raw string) bool {
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "data:") {
		if !strings.HasPrefix(lower, "data:image/") {
			return false
		}
		meta, _, ok := strings.Cut(raw[len("data:"):], ",")
		if !ok || !strings.HasSuffix(strings.ToLower(meta), ";base64") {
			return false
		}
		mime := meta[:len(meta)-len(";base64")]
		return len(mime) > len("image/")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		// URL userinfo 会被 net/http 转成 Basic Authorization 头；探测取图承诺
		// 全新空头，不接受 URL 内嵌凭据（prober 仍会防御性清空）。
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// probeInput 解析 input 三态：nil（缺省/null）→ 格式默认值；显式值先
// TrimSpace，空 → probe input is empty，trim 后 UTF-8 字节数 >8192 → probe
// input exceeds 8192 bytes，其余用 trim 后的值。默认值依赖 images mode。
func probeInput(format domain.RequestFormat, imageMode string, raw *string) (string, error) {
	if raw == nil {
		return probeDefaultInput(format, imageMode), nil
	}
	v := strings.TrimSpace(*raw)
	if v == "" {
		return "", fmt.Errorf("%w: %s", ErrInvalidInput, domain.ProbeErrorInputEmpty)
	}
	if len(v) > domain.ProbeMaxInputLen {
		return "", fmt.Errorf("%w: %s", ErrInvalidInput, domain.ProbeErrorInputTooLarge)
	}
	return v, nil
}

// probeDefaultInput 是探测缺省输入的唯一表（spec §4「输入默认值」）。
func probeDefaultInput(format domain.RequestFormat, imageMode string) string {
	switch format {
	case domain.FormatOpenAIImages:
		if imageMode == domain.ProbeImageModeEdits {
			return domain.ProbeDefaultInputEdits
		}
		return domain.ProbeDefaultInputImages
	case domain.FormatOpenAISearch:
		return domain.ProbeDefaultInputSearch
	default:
		return domain.ProbeDefaultInputText
	}
}
