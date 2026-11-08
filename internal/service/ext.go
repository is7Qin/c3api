// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
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

// transportDowngradeConfirm 降级确认令牌（spec §4.3 第 3 步）：把
// codex_responses_transport 从 ws 换成别的值必须显式带这个字面量
// （`?confirm=transport-downgrade`）。只认全等——"yes"、大小写变体、带空白都
// 不放行，避免"顺手带个 confirm 就过"。
const transportDowngradeConfirm = "transport-downgrade"

// upstreamPathRE 白名单：黑名单枚举不出 `\`、`;`、`%00` 等分隔符变体。
var upstreamPathRE = regexp.MustCompile(`^(?:v1/)?[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)*(?:\?[A-Za-z0-9._~=&%-]+)?$`)

// upstreamQueryRE 解码后的 query 白名单（`%` 不再允许——已解码完成）。
var upstreamQueryRE = regexp.MustCompile(`^[A-Za-z0-9._~=&-]+$`)

// validateUpstreamPaths 校验 custom 的逐格式路径：键必须属于 7 键全集（**不要求**
// 对应格式在 supported_formats 内——见下）；值按固定顺序规范化——trim → 剥至多
// 一个前导 `/` → 长度 1..1024 → 白名单；query 非空且解码后过白名单；任何路径段
// 不得为 `.` 或 `..`。返回**归一后**的 map（调用方必须用它落库）。
// nil/空 map = 未配置（合法）。
//
// 存下来的值是**原样拼到模板 base_url 之后**的自定义路径：这里不做任何协议补全
// （比如不因为缺 v1 就补上），补 /v1 只发生在「未配置 → 回落内置默认端点」那条
// 路上，两者互不干扰。
//
// 为什么不与 supported_formats 联动：本接口是全列替换，若拒绝「当前格式集之外」
// 的键，那么运维通过 API 收窄格式后，任何一次普通保存（哪怕只改个名字）都会把
// 这些残留覆盖永久删掉——而它们本应作为死配置保留、放宽后复活。键与格式的映射
// 是固定的，非当前格式的键只是永不生效，不构成可用性风险。
func validateUpstreamPaths(paths map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	for k, raw := range paths {
		if _, ok := upstreamPathFormat[k]; !ok {
			return nil, ErrInvalidInput
		}
		v := strings.TrimSpace(raw)
		v = strings.TrimPrefix(v, "/") // 至多剥一个（`//x` 仍会被白名单拒）
		if len(v) < 1 || len(v) > upstreamPathMaxLen {
			return nil, ErrInvalidInput
		}
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
		// 归一结果落库：剥前导斜杠/首尾空白只是把同一落点的多种写法收敛成一种
		// （值是原样拼到 base_url 之后的自定义路径，"/v1/x" 与 "v1/x" 打出去
		// 相同），避免库里留下「看着不同、效果一样」的重复值。
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
//
// 出站传输的判定顺序**本身是契约**（spec-codex-outbound-transport §4.3）：
//  0. 类型存在性（validateTemplateExt；必须先于降级门——api_key 模板根本没有
//     ext 行，不先拦就会先吃 409 再吃 400）；
//  1. 值域（!new.Valid() ⇒ 400；不 trim、不做大小写归一——回落会让两条实验臂
//     都落 http，读数"一致地看不出差别"，是事后最难定位的故障）；
//  2. 前置读旧值：GetTemplateExt 的 ErrNotFound ⇒ 默认面（首次建行不是 404）；
//  3. 类型（!new.AllowedFor(ct) && new != old ⇒ 400；**幂等例外** new == old
//     放行——模板类型改掉后陈旧 ext 行原样带回时不该报错）；
//  4. 降级门（old == ws && new != ws ⇒ 409，除非 confirm 恰为
//     transportDowngradeConfirm）。**不引入 expect_old**：需要确认的唯一情形
//     就是 old == ws，期望值恒为常量，绑不住任何东西；
//  5. 放行降级时 WARN（template_id/old/new）——没有指标系统，这是"实验臂何时
//     被降级"的唯一事后证据。
//
// confirm 只由 handler 从 `?confirm=` 解析后原样透传，判定只在本方法一处。
//
// ext 行是模板快照的静态原料之一（strip_image_tools 参与路由属性推导），
// 故写入后必须与模板其余写面同规失效：Templates() 全量重载调度快照 + 失效
// 客户端工厂，并广播 NOTIFY 使多实例收敛。
func (s *Service) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt, confirm string) (*domain.TemplateExt, error) {
	tpl, err := s.store.GetTemplate(ctx, e.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != e.CredentialType {
		return nil, ErrInvalidInput // 父模板类型与 ext 行类型必须一致
	}
	if err := validateTemplateExt(e); err != nil { // 第 0 步：类型存在性
		return nil, err
	}
	newTransport := e.CodexResponsesTransport
	if !newTransport.Valid() { // 第 1 步：值域（不 trim、不归一）
		return nil, ErrInvalidInput
	}
	// 第 2 步：前置读旧值。无 ext 行 = 首次建行 ⇒ 默认面（不是 404）。
	oldTransport := domain.CodexResponsesTransportDefault
	oldExt, err := s.store.GetTemplateExt(ctx, e.TemplateID)
	switch {
	case err == nil:
		oldTransport = oldExt.CodexResponsesTransport
	case errors.Is(err, repository.ErrNotFound):
		// 首次建行：old 取默认面
	default:
		return nil, mapRepoErr(err)
	}
	// 第 3 步：类型面。幂等例外：原值原样带回（new == old）放行——模板类型改
	// 掉后陈旧 ext 行（如 custom 行上残留的 ws）再保存时不该变成 400。
	if newTransport != oldTransport && !newTransport.AllowedFor(e.CredentialType) {
		return nil, ErrInvalidInput
	}
	// 第 4 步：降级门。old == ws 时把 ws 换成任何别的值都要显式确认：一次只改
	// strip_image_tools 的外部脚本会把 ws 静默清回默认，而 web 侧的回显门管
	// 不到脚本/curl/DB 直改——只有服务端能管住全部写面。
	if oldTransport == domain.CodexResponsesTransportWS && newTransport != domain.CodexResponsesTransportWS {
		if confirm != transportDowngradeConfirm {
			return nil, fmt.Errorf("%w: codex_responses_transport %q → %q 会让该模板的原生 responses 车道回落 http；确认降级请带 ?confirm=%s 重发",
				ErrConflict, string(oldTransport), string(newTransport), transportDowngradeConfirm)
		}
		// 第 5 步：放行降级时的唯一事后证据（审计面 = WARN 日志，spec §4.3）。
		// s.log 可能为 nil（测试/降级装配），守卫与其余写面一致。
		if s.log != nil {
			s.log.Warn("codex responses transport downgraded",
				logx.Int64("template_id", e.TemplateID),
				logx.String("old", string(oldTransport)),
				logx.String("new", string(newTransport)))
		}
	}
	if len(e.UpstreamPaths) > 0 {
		if e.CredentialType != credential.TypeCustom {
			return nil, ErrInvalidInput // 仅 custom 可配路径
		}
		normalized, err := validateUpstreamPaths(e.UpstreamPaths)
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
