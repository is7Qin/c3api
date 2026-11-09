// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
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
//  2. 类型（!new.AllowedFor(ct) && new != old ⇒ 400；**幂等例外** new == old
//     放行——模板类型改掉后陈旧 ext 行原样带回时不该报错）；
//  3. 降级门（old == ws && new != ws ⇒ 409，除非 confirm 恰为
//     transportDowngradeConfirm）。消息是**英文**短句（会原样进响应体，与既有
//     ErrConflict 同风格）。**不引入 expect_old**：需要确认的唯一情形就是
//     old == ws，期望值恒为常量，绑不住任何东西；
//  4. 落库（守卫写：事务内读-判-写 + 父行锁）；
//  5. 放行降级且**落库成功之后**写 WARN（template_id/old/new）——没有指标系统，
//     这是"实验臂何时被降级"的唯一事后证据；写在落库之前会在写失败时留下一条
//     假的"已降级"证据，审计必须反映事实。
//
// ⚠ 第 2/3 步的 `old` 唯一合法来源是**守卫写事务内那一次读**（spec §4.3 原子性
// 约束）：本方法**不得**在这里另读一次 ext 行来判定——事务外读到的旧值只允许
// 用于快速失败/回显，否则并发写可以绕过降级门（A 读到 http → B 提交 ws → A 无条件
// 写，实际完成 ws→http，既不 409 也不 WARN）。
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
	if !e.CodexResponsesTransport.Valid() { // 第 1 步：值域（不 trim、不归一）
		return nil, ErrInvalidInput
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
	// 第 2/3/4 步：交给守卫写在**同一事务**里完成（父行锁 + 事务内读 + decide +
	// 写）。old 经闭包捕获出来供第 5 步 WARN 用——这是 decide 唯一被允许的副作用。
	var oldTransport domain.CodexResponsesTransport
	saved, down, err := s.store.UpsertTemplateExt(ctx, e, func(old *domain.TemplateExt) (bool, error) {
		return decideTransport(e, old, confirm, &oldTransport)
	})
	if err != nil {
		// ErrConflict 必须原样透出：mapRepoErr 只重包 repository 的哨兵，
		// service 的 409 因此逐字到达 handler 的 errors.Is。
		return nil, mapRepoErr(err)
	}
	// 第 5 步：放行的降级，**落库成功之后**才写 WARN——落在写之前会在写失败时
	// 留下一条假的"已降级"证据，而审计必须反映事实（spec §4.3 第 5 步）。
	// old 取事务内那一次读（闭包捕获），new 取落库返回值：回显入参可能与实际
	// 落库值不一致。s.log 可能为 nil（测试/降级装配），守卫与其余写面一致。
	if down && s.log != nil {
		s.log.Warn("codex responses transport downgraded",
			logx.Int64("template_id", saved.TemplateID),
			logx.String("old", string(oldTransport)),
			logx.String("new", string(saved.CodexResponsesTransport)))
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return saved, nil
}

// decideTransport 是 §4.3 第 2/3 步的**唯一**判定实现（守卫写的事务内回调）：
// 入参只有**事务内**读到的 old（首次建行 = nil）、请求的 new（在 e 上）与 confirm。
//
// 纯函数：不访问 store/DB/日志——它被调用在已持有父行锁的事务里，回头读库等于
// 等自己持有的锁。唯一的副作用是把事务内那一次读到的旧值写到 oldOut（调用方
// 用它写 WARN；nil 表示调用方不需要）。
//
// 类型门（第 2 步，含幂等例外）先于降级门（第 3 步）：顺序本身是契约——值域与
// 类型检查都不通过时不得先返回 409。旧值用库中原值比较、**不归一**：脏值（"WS"
// 等）既不触发 409 也不享受幂等例外。
func decideTransport(e *domain.TemplateExt, old *domain.TemplateExt, confirm string, oldOut *domain.CodexResponsesTransport) (down bool, err error) {
	oldTransport := domain.CodexResponsesTransportDefault
	if old != nil {
		oldTransport = old.CodexResponsesTransport
	}
	if oldOut != nil {
		*oldOut = oldTransport
	}
	newTransport := e.CodexResponsesTransport
	// 第 2 步：类型面。幂等例外：原值原样带回（new == old）放行——模板类型改
	// 掉后陈旧 ext 行（如 custom 行上残留的 ws）再保存时不该变成 400。
	if newTransport != oldTransport && !newTransport.AllowedFor(e.CredentialType) {
		return false, ErrInvalidInput
	}
	// 第 3 步：降级门。old == ws 时把 ws 换成任何别的值都要显式确认：一次只改
	// strip_image_tools 的外部脚本会把 ws 静默清回默认，而 web 侧的回显门管
	// 不到脚本/curl/DB 直改——只有服务端能管住全部写面。
	// down 是第 5 步 WARN 的触发条件（不是"old == ws"：ws → ws 不是降级）。
	if oldTransport == domain.CodexResponsesTransportWS && newTransport != domain.CodexResponsesTransportWS {
		if confirm != transportDowngradeConfirm {
			// 消息是**英文**短句（与既有 ErrConflict 同风格）：它会原样进 API 响应体。
			return false, fmt.Errorf("%w: codex_responses_transport %s -> %s is a downgrade to http; resend with ?confirm=%s to confirm",
				ErrConflict, string(oldTransport), string(newTransport), transportDowngradeConfirm)
		}
		return true, nil
	}
	return false, nil
}
