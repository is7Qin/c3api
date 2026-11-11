// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
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
// 类型搬运，service 只认 domain.RequestFormat）。
type AccountProbeRequest struct {
	Model  string
	Format domain.RequestFormat
}

// probeFormats 可探测格式集合：openai-images/openai-search 明确不可测（400）。
var probeFormats = map[domain.RequestFormat]struct{}{
	domain.FormatOpenAIChat:        {},
	domain.FormatOpenAIResponses:   {},
	domain.FormatOpenAIResponsesWS: {},
	domain.FormatAnthropic:         {},
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
	acc, err := s.store.GetAccountWithTemplate(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	tpl := acc.Template
	if tpl == nil {
		return nil, fmt.Errorf("service: account %d has no template", id)
	}

	format := req.Format
	model := req.Model
	if _, ok := probeFormats[format]; !ok {
		return nil, fmt.Errorf("%w: invalid probe format %q", ErrInvalidInput, format)
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: probe model is empty", ErrInvalidInput)
	}
	if !tpl.FormatSupports(format, model) || !(tpl.Serves(model) || !tpl.HasModelSpace()) {
		return nil, fmt.Errorf("%w: model %q is not eligible for template format %q", ErrInvalidInput, model, format)
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
		ext, xerr := s.store.GetTemplateExt(ctx, tpl.ID)
		switch {
		case xerr == nil:
			if ext != nil {
				upstreamPaths = ext.UpstreamPaths
			}
		case errors.Is(xerr, repository.ErrNotFound):
			// 无 ext 行 = 未配置。
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

	var (
		token          string
		codexAccountID string
	)
	if tpl.CredentialType.IsCodex() {
		cred, cerr := s.store.GetOwnedAccountCredential(ctx, id)
		if cerr != nil {
			if errors.Is(cerr, repository.ErrNotFound) {
				return nil, fmt.Errorf("%w: 账号缺少 codex 凭据", ErrInvalidInput)
			}
			return nil, cerr
		}
		switch tpl.CredentialType {
		case credential.TypeCodexOAuth:
			if cred.OAuthToken == "" {
				return nil, fmt.Errorf("%w: 账号缺少 codex 凭据", ErrInvalidInput)
			}
			// 静态探测不能刷新：过期直接给出结论（刷新会轮换 RT 且本切片不落库
			// ⇒ 会毁掉账号），绝不发网络请求。
			if cred.OAuthExpiresAt != nil && !cred.OAuthExpiresAt.After(time.Now()) {
				res := domain.ProbeResult{
					Format:         format,
					RequestedModel: model,
					UpstreamModel:  upstreamModel,
					Error:          "codex access token expired; re-run device login",
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
	}
	if acc.Proxy != nil {
		spec.ProxyPassword = acc.Proxy.Password
	}
	res := s.prober.Probe(ctx, spec)
	s.logProbe(id, res)
	return &res, nil
}

// logProbe 每个探测恰好一条 Info（§7）：不含凭据、不含响应体。失败另记已脱敏
// error（prober 已脱敏/截断）。
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
