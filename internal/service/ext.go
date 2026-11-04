// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
)

// —— 模板类型化扩展（template_ext 1:1；通用框架：表结构/CRUD 骨架/类型枚举
// 校验。数据层 CRUD + 契约；codex 专属类型/列组见
// ext_codex.go，未来 claude oauth 等新类型 → 新增 ext_claude.go 同构） ——

// upstreamPathFormat 自定义路径键 → 需要的格式（键集合固定，见 spec §4.3）。
var upstreamPathFormat = map[string]domain.RequestFormat{
	"openai-chat":               domain.FormatOpenAIChat,
	"openai-responses":          domain.FormatOpenAIResponses,
	"openai-responses-ws":       domain.FormatOpenAIResponsesWS,
	"anthropic":                 domain.FormatAnthropic,
	"openai-search":             domain.FormatOpenAISearch,
	"openai-images.generations": domain.FormatOpenAIImages,
	"openai-images.edits":       domain.FormatOpenAIImages,
}

const upstreamPathMaxLen = 1024

// upstreamPathRE 白名单：黑名单枚举不出 `\`、`;`、`%00` 等分隔符变体。
var upstreamPathRE = regexp.MustCompile(`^(?:v1/)?[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)*(?:\?[A-Za-z0-9._~=&%-]+)?$`)

// upstreamQueryRE 解码后的 query 白名单（`%` 不再允许——已解码完成）。
var upstreamQueryRE = regexp.MustCompile(`^[A-Za-z0-9._~=&-]+$`)

// validateUpstreamPaths 校验 custom 的逐格式路径：键必须属于全集且对应格式在
// supported_formats 内；值长度 1..1024、去至多一个前导 `/` 后过白名单、
// query 非空且解码后过白名单、任何路径段不得为 `.` 或 `..`。
// nil/空 map = 未配置（合法）。
func validateUpstreamPaths(paths map[string]string, supported map[domain.RequestFormat]bool) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	for k, raw := range paths {
		f, ok := upstreamPathFormat[k]
		if !ok {
			return nil, ErrInvalidInput
		}
		if supported != nil && !supported[f] {
			return nil, ErrInvalidInput
		}
		v := strings.TrimSpace(raw)
		if len(v) < 1 || len(v) > upstreamPathMaxLen {
			return nil, ErrInvalidInput
		}
		v = strings.TrimPrefix(v, "/")
		if v == "" || !upstreamPathRE.MatchString(v) {
			return nil, ErrInvalidInput
		}
		pathPart, query := v, ""
		if i := strings.IndexByte(v, '?'); i >= 0 {
			pathPart, query = v[:i], v[i+1:]
			if query == "" {
				return nil, ErrInvalidInput
			}
			dec, err := url.QueryUnescape(query)
			if err != nil || !upstreamQueryRE.MatchString(dec) {
				return nil, ErrInvalidInput
			}
		}
		for _, seg := range strings.Split(pathPart, "/") {
			if seg == "." || seg == ".." {
				return nil, ErrInvalidInput
			}
		}
		// 归一结果落库：只校验不归一，会让 "/v1/messages" 或带空白的值通过校验
		// 却在上游拼成 /v1/v1/messages、%20...，运维得到一个 404 模板。
		out[k] = v
	}
	return out, nil
}

// validateTemplateExt 校验模板 ext 行：credential_type ∈ {responses-special,
// codex-oauth, codex-pat, custom}（api_key 主列类型无 ext 行；类型一致性——
// ext 行类型必须 == 父模板类型——由 UpsertTemplateExt 校验）。模板是共享配置面：
// 可空列 strip_image_tools（四类型公共能力开关，nil = 未配置 = 关闭）与
// upstream_paths（仅 custom 有意义，nil = 全部按格式默认端点）。
func validateTemplateExt(e *domain.TemplateExt) error {
	if e.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if !e.CredentialType.ValidTemplateExt() {
		return ErrInvalidInput
	}
	return nil
}

// GetTemplateExt 模板 ext 行（编辑回显）。模板缺 id → 404。
func (s *Service) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	if _, err := s.store.GetTemplate(ctx, templateID); err != nil {
		return nil, mapRepoErr(err)
	}
	e, err := s.store.GetTemplateExt(ctx, templateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return e, nil
}

// UpsertTemplateExt 幂等写入模板 ext 行（Create/Update 合一；update 全列更新
// 含 NULL 清空）。模板缺 id → 404（FK 由仓库保证）。
// 类型一致性：ext 行 credential_type 必须与父模板的 credential_type 一致
// （api_key 模板无 ext 行；special/oauth/pat 模板只能挂同类型行）——不一致 → 400。
// ext 行是模板快照的静态原料之一（strip_image_tools 参与路由属性推导），
// 故写入后必须与模板其余写面同规失效：Templates() 全量重载调度快照 + 失效
// 客户端工厂，并广播 NOTIFY 使多实例收敛。
func (s *Service) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error) {
	tpl, err := s.store.GetTemplate(ctx, e.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != e.CredentialType {
		return nil, ErrInvalidInput // 父模板类型与 ext 行类型必须一致
	}
	if err := validateTemplateExt(e); err != nil {
		return nil, err
	}
	if len(e.UpstreamPaths) > 0 {
		if e.CredentialType != credential.TypeCustom {
			return nil, ErrInvalidInput // 仅 custom 可配路径
		}
		supported := make(map[domain.RequestFormat]bool, len(tpl.SupportedFormats))
		for _, f := range tpl.SupportedFormats {
			supported[f] = true
		}
		normalized, err := validateUpstreamPaths(e.UpstreamPaths, supported)
		if err != nil {
			return nil, err
		}
		e.UpstreamPaths = normalized
	} else {
		// 空 map 与 nil 同义（都表示「未配置任何覆盖」）：归一到 nil，让「清空」
		// 落成 JSON NULL 而不是 `{}`——否则同一语义有两种落库形态，回显与
		// 运维排查会出现「明明清空了却还看到 {}」的噪音。
		e.UpstreamPaths = nil
	}
	saved, err := s.store.UpsertTemplateExt(ctx, e)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return saved, nil
}
