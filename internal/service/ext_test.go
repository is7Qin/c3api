// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// seedTemplate 建模板（格式/类型可自选；缺省 api_key + openai-chat）。
func seedExtTemplate(t *testing.T, svc *Service, name string, ct credential.Type, formats ...domain.RequestFormat) *domain.Template {
	t.Helper()
	if len(formats) == 0 {
		formats = []domain.RequestFormat{domain.FormatOpenAIChat}
	}
	tpl, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: "https://u", CredentialType: ct, SupportedFormats: formats,
	})
	require.NoError(t, err)
	return tpl
}

// TestTemplateFormatEnum 格式枚举白名单：openai-responses-ws 合法；未知值 400
// （resp-ws 为枚举值，非独立字段）。
func TestTemplateFormatEnum(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// resp-ws 单独/混用均合法（api_key 类型四格式任意）
	for i, fmts := range [][]domain.RequestFormat{
		{domain.FormatOpenAIResponsesWS},
		{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS},
		{domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS, domain.FormatAnthropic},
	} {
		_, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t" + strconv.Itoa(i), BaseURL: "https://u", SupportedFormats: fmts,
		})
		require.NoError(t, err, "formats %v must be valid", fmts)
	}

	// 未知格式值 → 400
	_, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "bad", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{"resp-ws"},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "裸 resp-ws（非枚举值）必须 400")
	_, err = svc.CreateTemplate(ctx, &domain.Template{
		Name: "bad2", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{"bogus"},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "未知格式必须 400")
}

// TestTemplateCredentialTypeConstraint 类型-格式约束：special/oauth/pat 模板
// 只允许 resp/resp-ws（resp-ws 可选）；api_key 四格式任意；credential_type
// 白名单（未知 → 400）。
func TestTemplateCredentialTypeConstraint(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// 生态三类型 + resp/resp-ws 组合均合法
	formatsCases := [][]domain.RequestFormat{
		{domain.FormatOpenAIResponses},
		{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS},
		{domain.FormatOpenAIResponsesWS},
	}
	for _, ct := range []credential.Type{credential.TypeResponsesSpecial, credential.TypeCodexOAuth, credential.TypeCodexPAT} {
		for i, fmts := range formatsCases {
			_, err := svc.CreateTemplate(ctx, &domain.Template{
				Name: string(ct) + "-" + strconv.Itoa(i), BaseURL: "https://u",
				CredentialType: ct, SupportedFormats: fmts,
			})
			require.NoError(t, err, "type %s formats %v must be valid", ct, fmts)
		}
		// 非 resp 格式 → 400
		for _, f := range []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic} {
			_, err := svc.CreateTemplate(ctx, &domain.Template{
				Name: string(ct) + "-bad", BaseURL: "https://u",
				CredentialType: ct, SupportedFormats: []domain.RequestFormat{f},
			})
			require.ErrorIs(t, err, ErrInvalidInput, "type %s format %s must be rejected", ct, f)
		}
	}

	// credential_type 白名单：未知值 → 400
	_, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "unknown-ct", BaseURL: "https://u",
		CredentialType: credential.Type("codex_oauth"), SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "未知 credential_type 必须 400")
}

// TestUpdateTemplateTypeFormatConstraint PUT 更新同样受约束（新建合法 → 改成
// 非 resp 格式 → 400；类型改成生态三类型 + chat 格式 → 400）。
func TestUpdateTemplateTypeFormatConstraint(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)

	// 生态类型模板改成 chat → 400
	tpl.SupportedFormats = []domain.RequestFormat{domain.FormatOpenAIChat}
	_, err := svc.UpdateTemplate(ctx, tpl)
	require.ErrorIs(t, err, ErrInvalidInput)

	// api_key 模板四格式任意（含 resp-ws）
	apiTpl := seedExtTemplate(t, svc, "t2", credential.TypeAPIKey, domain.FormatOpenAIChat)
	apiTpl.SupportedFormats = []domain.RequestFormat{domain.FormatOpenAIResponsesWS, domain.FormatAnthropic}
	_, err = svc.UpdateTemplate(ctx, apiTpl)
	require.NoError(t, err)
}

// TestTemplateExtValidation ext 行校验：类型白名单（模板三类型；api_key 拒绝）
// + 类型一致性（ext 行类型必须 == 父模板类型；special 模板挂 oauth/pat 行 →
// 400）+ strip_image_tools 三类型公共能力 roundtrip（幂等 upsert + NULL 清空）
// + 父行缺失 404。模板 ext 无凭据列（oauth/pat 一律在 account_ext）。
func TestTemplateExtValidation(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-special", credential.TypeResponsesSpecial, domain.FormatOpenAIResponses)

	// special 行：strip_image_tools 公共开关 roundtrip
	saved, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeResponsesSpecial,
		StripImageTools: boolPtr(true),
	})
	require.NoError(t, err)
	require.Equal(t, tpl.ID, saved.TemplateID)
	require.Equal(t, credential.TypeResponsesSpecial, saved.CredentialType)
	require.NotNil(t, saved.StripImageTools)
	require.True(t, *saved.StripImageTools)

	// 类型一致性：special 模板挂 oauth/pat 行 → 400（类型与父模板不一致）
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "special 模板 ext 行类型必须一致（oauth 拒绝）")
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexPAT,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "special 模板 ext 行类型必须一致（pat 拒绝）")

	// oauth 模板：strip 开关同可用（三类型公共能力）+ 幂等 upsert 覆盖（NULL 清空）
	tplO := seedExtTemplate(t, svc, "t-oauth", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	saved, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tplO.ID, CredentialType: credential.TypeCodexOAuth,
		StripImageTools: boolPtr(true),
	})
	require.NoError(t, err)
	require.True(t, *saved.StripImageTools, "oauth 模板 strip 开关可配置（三类型公共能力）")

	got, err := svc.GetTemplateExt(ctx, tplO.ID)
	require.NoError(t, err)
	require.True(t, *got.StripImageTools, "roundtrip")

	// 幂等 upsert：同 template_id 再写（strip=false）→ 仍单行、值覆盖
	saved, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tplO.ID, CredentialType: credential.TypeCodexOAuth,
		StripImageTools: boolPtr(false),
	})
	require.NoError(t, err)
	require.False(t, *saved.StripImageTools, "幂等 upsert 覆盖（改值）")

	// NULL 清空：写 nil → 落 NULL
	saved, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tplO.ID, CredentialType: credential.TypeCodexOAuth,
	})
	require.NoError(t, err)
	require.Nil(t, saved.StripImageTools, "nil 显式清列（NULL 落库）")

	// oauth 模板挂 pat 行 → 400（类型不一致）
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tplO.ID, CredentialType: credential.TypeCodexPAT,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "oauth 模板 ext 行类型必须一致（pat 拒绝）")

	// pat 模板 + pat 行：roundtrip（strip 同样可用）
	tplP := seedExtTemplate(t, svc, "t-pat", credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	saved, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tplP.ID, CredentialType: credential.TypeCodexPAT,
		StripImageTools: boolPtr(true),
	})
	require.NoError(t, err)
	require.True(t, *saved.StripImageTools, "pat 模板 strip 开关可配置")
	got, err = svc.GetTemplateExt(ctx, tplP.ID)
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexPAT, got.CredentialType)

	// api_key 类型 → 400（主列类型无 ext 行）
	apiTpl := seedExtTemplate(t, svc, "t-key", credential.TypeAPIKey, domain.FormatOpenAIChat)
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: apiTpl.ID, CredentialType: credential.TypeAPIKey,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "api_key 类型模板不允许 ext 行")

	// 父模板缺失 → 404
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: 999, CredentialType: credential.TypeResponsesSpecial,
	})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.GetTemplateExt(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound)

	// 无 ext 行 → 404
	_, err = svc.GetTemplateExt(ctx, apiTpl.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestAccountExtValidation 账号 ext：类型白名单（只 codex-oauth/codex-pat；
// special/api_key 拒绝）+ 类型一致性（ext 行类型必须 == 父模板类型；oauth 模板
// 账号挂 pat 行 / api_key 模板账号挂 codex 行 → 400）+ 列组约束 + roundtrip
// （身份/email 持久复用）+ 父行缺失 404。
func TestAccountExtValidation(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tplO := seedExtTemplate(t, svc, "t-oauth", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	accO := seedExtAccount(t, svc, tplO.ID)
	tplP := seedExtTemplate(t, svc, "t-pat", credential.TypeCodexPAT, domain.FormatOpenAIResponses)
	accP := seedExtAccount(t, svc, tplP.ID)

	const iid = "11111111-2222-3333-4444-555555555555"
	exp := time.Now().Add(time.Hour)

	// oauth 账号：首次写入缺省身份 → service 自动生成四元组（NewCodexIdentity）：
	// installation UUIDv4 形状；session==thread（主线程语义）；window={thread_id}:0
	// 恒定；email 非自动生成（人工/上游导入）。
	saved, err := svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accO.ID, CredentialType: credential.TypeCodexOAuth,
		OAuthToken: strPtr("at"), OAuthRefreshToken: strPtr("rt"), OAuthExpiresAt: &exp,
	})
	require.NoError(t, err)
	require.Equal(t, "at", *saved.OAuthToken)
	require.NotEmpty(t, saved.InstallationID, "首次写入自动生成 installation_id")
	require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
		saved.InstallationID, "installation_id UUIDv4 形状")
	require.NotNil(t, saved.SessionID)
	require.NotNil(t, saved.ThreadID)
	require.Equal(t, saved.SessionID, saved.ThreadID, "主线程 thread_id == session_id（真实客户端语义）")
	require.Equal(t, *saved.ThreadID+":0", *saved.WindowID, "window_id = {thread_id}:0（恒定）")
	require.Nil(t, saved.Email, "email 非自动生成（NewCodexIdentity 只生成身份四元组）")
	autoIID := saved.InstallationID

	got, err := svc.GetAccountExt(ctx, accO.ID)
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexOAuth, got.CredentialType)
	require.Equal(t, "rt", *got.OAuthRefreshToken)
	require.Equal(t, autoIID, got.InstallationID)
	require.Nil(t, got.Email, "未提供 email → NULL 落库")

	// 后续写入缺省身份 → 沿用存量（持久复用，账号存在期间稳定）+ 缺省列清空
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accO.ID, CredentialType: credential.TypeCodexOAuth, OAuthToken: strPtr("at2"),
	})
	require.NoError(t, err)
	require.Equal(t, "at2", *saved.OAuthToken)
	require.Nil(t, saved.OAuthRefreshToken, "缺省列 NULL 清空")
	require.Equal(t, autoIID, saved.InstallationID, "installation_id 持久复用")
	require.Equal(t, *saved.ThreadID+":0", *saved.WindowID, "window 持久复用恒定")

	// 类型一致性：oauth 模板账号挂 pat 行 → 400（父模板类型不一致）
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accO.ID, CredentialType: credential.TypeCodexPAT, InstallationID: iid, PATKey: strPtr("pat"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "oauth 模板账号 ext 行类型必须一致（pat 拒绝）")

	// pat 账号：显式身份 + email（导入时人工/上游填写）→ 采用；随后缺省沿用。
	// 恒等式：thread==session、window={thread}:0（I1）
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accP.ID, CredentialType: credential.TypeCodexPAT,
		InstallationID: iid, Email: strPtr("user@example.com"), PATKey: strPtr("pat"),
		SessionID: strPtr("s1"), ThreadID: strPtr("s1"), WindowID: strPtr("s1:0"),
	})
	require.NoError(t, err)
	require.Equal(t, iid, saved.InstallationID)
	require.Equal(t, "user@example.com", *saved.Email, "email roundtrip")
	require.Equal(t, "s1", *saved.SessionID)
	require.Equal(t, "s1", *saved.ThreadID, "thread==session 恒等")
	require.Equal(t, "s1:0", *saved.WindowID)
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accP.ID, CredentialType: credential.TypeCodexPAT, PATKey: strPtr("pat2"),
	})
	require.NoError(t, err)
	require.Equal(t, iid, saved.InstallationID, "显式提供后缺省沿用")
	require.Nil(t, saved.Email, "未提供 email → NULL 清空（B1-5：email 不在缺省沿用面）")
	require.Equal(t, "s1", *saved.SessionID, "session 持久复用")

	// 列组约束
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accO.ID, CredentialType: credential.TypeCodexOAuth, InstallationID: iid, PATKey: strPtr("p"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "oauth 行 pat 列必须为空")
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accP.ID, CredentialType: credential.TypeCodexPAT, InstallationID: iid, OAuthToken: strPtr("t"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "pat 行 oauth 列必须为空")

	// B1-2 复查落库态：校验先于落库——被拒写入零残留（存量行不被改动）
	got2, err := svc.GetAccountExt(ctx, accO.ID)
	require.NoError(t, err)
	require.Equal(t, "at2", *got2.OAuthToken, "被拒写入不得改动存量行")
	got2, err = svc.GetAccountExt(ctx, accP.ID)
	require.NoError(t, err)
	require.Equal(t, "pat2", *got2.PATKey)

	// oauth 最小完整性：三列全空 → 400（refresh/expires 可空——仅 token 已覆盖）
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accO.ID, CredentialType: credential.TypeCodexOAuth, InstallationID: iid,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "oauth 行至少 oauth_token")

	// B1-2 首写零残留：新账号被拒（列组违规）→ 无行落库（修复前 TryInsert
	// 先写、校验后置 → 被拒凭据残留进调度快照被真实使用）
	accBad := seedExtAccount(t, svc, tplP.ID)
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accBad.ID, CredentialType: credential.TypeCodexPAT,
		PATKey: strPtr("leaked-pat"), OAuthToken: strPtr("t"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "pat 行 oauth 列必须为空")
	_, err = svc.GetAccountExt(ctx, accBad.ID)
	require.ErrorIs(t, err, ErrNotFound, "被拒凭据零残留——校验失败不得落库")

	// B1-4 pat 最小完整性：pat 行必须 pat_key（与 oauth 分支对称——空 key 写
	// 成功即死账号 + 运行时误报失效）
	accP2 := seedExtAccount(t, svc, tplP.ID)
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accP2.ID, CredentialType: credential.TypeCodexPAT, InstallationID: iid,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "pat 行必须 pat_key")
	_, err = svc.GetAccountExt(ctx, accP2.ID)
	require.ErrorIs(t, err, ErrNotFound, "pat 空 key 被拒 → 零残留")

	// 类型白名单：responses-special 账号 ext → 400；api_key 模板账号挂 codex
	// 行 → 400（父模板类型不一致）
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accO.ID, CredentialType: credential.TypeResponsesSpecial, InstallationID: iid,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "账号 ext 不接受 special 类型")
	apiTpl := seedExtTemplate(t, svc, "t-key", credential.TypeAPIKey, domain.FormatOpenAIChat)
	accKey := seedExtAccount(t, svc, apiTpl.ID)
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accKey.ID, CredentialType: credential.TypeCodexOAuth, InstallationID: iid,
	})
	require.ErrorIs(t, err, ErrInvalidInput, "api_key 模板账号不允许 codex ext 行")

	// B1-5 email 清空往返：提供 → 写入；未提供 → NULL 清空（全列更新含
	// NULL 清空契约；修复前 fillIdentityDefaults 恒回填存量 email → 不可清空）
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accP.ID, CredentialType: credential.TypeCodexPAT, PATKey: strPtr("pat3"),
		Email: strPtr("again@example.com"),
	})
	require.NoError(t, err)
	require.Equal(t, "again@example.com", *saved.Email, "提供 email → 写入")
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accP.ID, CredentialType: credential.TypeCodexPAT, PATKey: strPtr("pat3"),
	})
	require.NoError(t, err)
	require.Nil(t, saved.Email, "未提供 email → NULL 清空（不再持久复用）")
	got, err = svc.GetAccountExt(ctx, accP.ID)
	require.NoError(t, err)
	require.Nil(t, got.Email, "落库态复查：email 已清空")

	// 父账号缺失 → 404
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: 999, CredentialType: credential.TypeCodexOAuth, InstallationID: iid,
	})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.GetAccountExt(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestAccountExtIdentityInvariant 身份恒等式（I1）：thread==session、
// window={thread}:0（零透传）——显式部分提供自动补齐（只给 session → thread
// 恒等 + window 派生；只给 thread → session 跟随；只给 window → 反推
// thread/session）；成对显式冲突 / window 与 {thread}:0 不符 → 400。
func TestAccountExtIdentityInvariant(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tplO := seedExtTemplate(t, svc, "t-oauth", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	acc := seedExtAccount(t, svc, tplO.ID)

	// 只给 session → thread 自动补齐恒等 + window 派生 + installation 自动生成
	saved, err := svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		SessionID: strPtr("s1"), OAuthToken: strPtr("at"),
	})
	require.NoError(t, err)
	require.Equal(t, "s1", *saved.SessionID)
	require.Equal(t, "s1", *saved.ThreadID, "只给 session → thread 自动补齐恒等")
	require.Equal(t, "s1:0", *saved.WindowID, "window = {thread}:0 派生")
	require.NotEmpty(t, saved.InstallationID, "installation 缺省自动生成")

	// 已有行：只给 thread（轮换）→ session 跟随、window 跟随
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		ThreadID: strPtr("t2"), OAuthToken: strPtr("at"),
	})
	require.NoError(t, err)
	require.Equal(t, "t2", *saved.ThreadID)
	require.Equal(t, "t2", *saved.SessionID, "只给 thread → session 补齐恒等")
	require.Equal(t, "t2:0", *saved.WindowID, "window 跟随 thread 派生")

	// B1-3 方向 2：存量行只给 window——反推 == 存量 thread → 幂等保留；
	// 反推 ≠ 存量 → 400（派生值不得冒充显式值改身份）
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		WindowID: strPtr("t2:0"), OAuthToken: strPtr("at"),
	})
	require.NoError(t, err)
	require.Equal(t, "t2", *saved.ThreadID, "存量 window-only 反推 == 存量 → 保留")
	require.Equal(t, "t2:0", *saved.WindowID)
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		WindowID: strPtr("x9:0"), OAuthToken: strPtr("at"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "存量 window-only 反推 ≠ 存量 → 400")
	got, err := svc.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "t2", *got.ThreadID, "400 不改动存量身份")

	// 另一账号：只给 window → 反推 thread/session
	acc2 := seedExtAccount(t, svc, tplO.ID)
	saved, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc2.ID, CredentialType: credential.TypeCodexOAuth,
		WindowID: strPtr("w1:0"), OAuthToken: strPtr("at"),
	})
	require.NoError(t, err)
	require.Equal(t, "w1", *saved.ThreadID, "只给 window → 反推 thread")
	require.Equal(t, "w1", *saved.SessionID, "thread==session 恒等")
	require.Equal(t, "w1:0", *saved.WindowID)

	// 成对显式冲突：session ≠ thread → 400
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		SessionID: strPtr("s9"), ThreadID: strPtr("t9x"), OAuthToken: strPtr("at"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "session≠thread 成对冲突必须 400")

	// window 与 {thread}:0 不符（thread 已知）→ 400
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		ThreadID: strPtr("t3"), WindowID: strPtr("t3:5"), OAuthToken: strPtr("at"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "window 非 {thread}:0 必须 400")

	// 只给 window 且形状非法 → 400
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		WindowID: strPtr(":0"), OAuthToken: strPtr("at"),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "window 形状非法必须 400")
}

// TestAccountExtConcurrentFirstWrite 首写原子性（I2）：并发双导入同一账号
// （身份全缺省）→ 单份身份且不覆盖、不报错（先写者胜；后到者回读赢者沿用
// 身份写令牌）。
func TestAccountExtConcurrentFirstWrite(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-oauth", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	acc := seedExtAccount(t, svc, tpl.ID)

	const n = 8
	results := make([]*domain.AccountExt, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.UpsertAccountExt(ctx, &domain.AccountExt{
				AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
				OAuthToken: strPtr("at"),
			})
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "并发首写不报错")
		require.NotNil(t, results[i], "并发首写都有返回值")
	}
	got, err := svc.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		require.Equal(t, got.InstallationID, results[i].InstallationID, "单份身份不覆盖（i=%d）", i)
		require.Equal(t, *got.SessionID, *results[i].SessionID)
		require.Equal(t, *got.ThreadID, *results[i].ThreadID)
		require.Equal(t, *got.WindowID, *results[i].WindowID)
	}
	require.Equal(t, *got.ThreadID+":0", *got.WindowID, "恒等式 window={thread}:0")
	require.Equal(t, *got.SessionID, *got.ThreadID, "恒等式 thread==session")
}

// firstCallBarrierStore fakeStore 包装：对 GetAccountExt 的前 want 次调用设
// 屏障——先完成读，再等全部到齐后放行（之后不再拦）。并发首写测试中所有
// 参与者的首次取行都在任何 TryInsert 之前完成（读全部返回"无存量行"）→
// 冲突确定性发生在 TryInsert 路径（B1-3 方向 3 直测，避免参与者误入存量行
// 编辑路径——若只同步读起点，先到的读可抢先 TryInsert 落行，后到的读会看到
// 赢者行）。
type firstCallBarrierStore struct {
	*fakeStore
	mu     sync.Mutex
	arrive int
	want   int
	gate   chan struct{}
}

func newFirstCallBarrierStore(want int) *firstCallBarrierStore {
	return &firstCallBarrierStore{fakeStore: newFakeStore(), want: want, gate: make(chan struct{})}
}

func (f *firstCallBarrierStore) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	e, err := f.fakeStore.GetAccountExt(ctx, accountID)
	f.mu.Lock()
	if f.arrive < f.want {
		f.arrive++
		if f.arrive == f.want {
			close(f.gate)
			f.mu.Unlock()
		} else {
			f.mu.Unlock()
			<-f.gate
		}
	} else {
		f.mu.Unlock()
	}
	return e, err
}

// TestAccountExtConflictLoserAdoptsWinnerIdentity B1-3 方向 3：并发首写冲突
// 路径——败者完全采用赢者身份（显式身份只在首写成功路径生效）。败者带显式
// 身份输入（window-only 派生 / session 恒等），若以派生值覆盖赢者 → 身份
// 混搭（thread 来自 A、window 来自 B）→ 断言最终身份恒为单一完整四元组。
func TestAccountExtConflictLoserAdoptsWinnerIdentity(t *testing.T) {
	const n = 6
	svc := &Service{store: newFirstCallBarrierStore(n), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-oauth", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	acc := seedExtAccount(t, svc, tpl.ID)

	results := make([]*domain.AccountExt, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := &domain.AccountExt{
				AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
				OAuthToken: strPtr("at"),
			}
			if i%2 == 0 {
				e.WindowID = strPtr(fmt.Sprintf("w%d:0", i))
			} else {
				e.SessionID = strPtr(fmt.Sprintf("s%d", i))
			}
			results[i], errs[i] = svc.UpsertAccountExt(ctx, e)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "并发首写不报错")
	}
	got, err := svc.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, *got.SessionID, *got.ThreadID, "恒等式 thread==session")
	require.Equal(t, *got.ThreadID+":0", *got.WindowID, "恒等式 window={thread}:0")
	for i := 0; i < n; i++ {
		require.Equal(t, got.InstallationID, results[i].InstallationID, "败者 installation 必须完全采用赢者（i=%d）", i)
		require.Equal(t, *got.SessionID, *results[i].SessionID, "败者 session 必须完全采用赢者（i=%d）", i)
		require.Equal(t, *got.ThreadID, *results[i].ThreadID, "败者 thread 必须完全采用赢者（i=%d）", i)
		require.Equal(t, *got.WindowID, *results[i].WindowID, "败者 window 必须完全采用赢者（i=%d）", i)
		require.Equal(t, "at", *results[i].OAuthToken, "败者凭据（令牌）按本次请求写")
	}
}

// TestUpdateTemplatesBatchTypeFormatConstraint 批量更新类型-格式约束（W1；
// 批量查询改造回归）：special/oauth/pat 模板批量改非 resp 格式 → 400；api_key
// 模板任意格式合法；混合批任一违规即拒（先于任何更新）；缺 id → 404（批量
// IN 查询数量对比拦截）。
func TestUpdateTemplatesBatchTypeFormatConstraint(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	special := seedExtTemplate(t, svc, "t-special", credential.TypeResponsesSpecial, domain.FormatOpenAIResponses)
	api := seedExtTemplate(t, svc, "t-key", credential.TypeAPIKey, domain.FormatOpenAIChat)

	chat := domain.FormatOpenAIChat
	resp := domain.FormatOpenAIResponses

	// special 模板批量改 chat → 400（resp-only 约束）
	err := svc.UpdateTemplatesBatch(ctx, []int64{special.ID},
		repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{chat}})
	require.ErrorIs(t, err, ErrInvalidInput, "special 模板批量改非 resp 格式必须 400")

	// api_key 模板批量改 chat → 合法（走通 store 落库）
	err = svc.UpdateTemplatesBatch(ctx, []int64{api.ID},
		repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{chat}})
	require.NoError(t, err, "api_key 模板任意格式合法")

	// 混合批：special + api_key 改 chat → 400（任一违规即拒）
	err = svc.UpdateTemplatesBatch(ctx, []int64{special.ID, api.ID},
		repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{chat}})
	require.ErrorIs(t, err, ErrInvalidInput, "混合批任一违规必须 400")

	// 混合批改 resp → 合法
	err = svc.UpdateTemplatesBatch(ctx, []int64{special.ID, api.ID},
		repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{resp}})
	require.NoError(t, err, "混合批全 resp 合法")

	// 缺 id（带 SupportedFormats）→ 404（批量查询数量对比拦截，先于更新）
	err = svc.UpdateTemplatesBatch(ctx, []int64{999},
		repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{resp}})
	require.ErrorIs(t, err, ErrNotFound, "缺 id → 404")
}

// TestNewCodexIdentity 身份四元组形状：installation UUIDv4、session/thread
// UUIDv7（版本位 7）、thread==session、window={thread}:0、两次生成不同。
func TestNewCodexIdentity(t *testing.T) {
	id1 := NewCodexIdentity()
	require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id1.InstallationID)
	for _, v := range []string{id1.SessionID, id1.ThreadID} {
		require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, v, "UUIDv7 形状")
	}
	require.Equal(t, id1.SessionID, id1.ThreadID, "主线程 thread_id == session_id")
	require.Equal(t, id1.ThreadID+":0", id1.WindowID)
	id2 := NewCodexIdentity()
	require.NotEqual(t, id1.InstallationID, id2.InstallationID, "每次生成新 installation")
	require.NotEqual(t, id1.SessionID, id2.SessionID)
}

// TestGroupProtocolConvert 分组 protocol_convert 方向集合：多方向 roundtrip +
// off 归一（空/仅 off → 空数组）+ 非法值/重复方向/同客户端格式冲突 400
// （create/update；显式空数组 = 清空）。
func TestGroupProtocolConvert(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// 多方向 roundtrip（off 元素归一剔除）
	g, err := svc.CreateGroup(ctx, "g-multi", domain.GroupVisibilityPublic, nil,
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp, domain.ProtocolConvertOff})
	require.NoError(t, err)
	got, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp},
		got.ProtocolConverts, "off 归一剔除、多方向落库")

	// off 归一空数组（nil / 仅 off / 空数组 同语义）
	for _, name := range []string{"g-nil", "g-off", "g-empty"} {
		var pcs []domain.ProtocolConvert
		switch name {
		case "g-off":
			pcs = []domain.ProtocolConvert{domain.ProtocolConvertOff}
		case "g-empty":
			pcs = []domain.ProtocolConvert{}
		}
		g0, err := svc.CreateGroup(ctx, name, domain.GroupVisibilityPublic, nil, pcs)
		require.NoError(t, err)
		got0, err := svc.GetGroup(ctx, g0.ID)
		require.NoError(t, err)
		require.Empty(t, got0.ProtocolConverts, "%s → 空数组 = 不转换", name)
	}

	// 非法值 → 400
	_, err = svc.CreateGroup(ctx, "bad", domain.GroupVisibilityPublic, nil,
		[]domain.ProtocolConvert{domain.ProtocolConvert("chat-to-resp")})
	require.ErrorIs(t, err, ErrInvalidInput, "连字符命名（chat-to-resp）非法，枚举用下划线")
	_, err = svc.CreateGroup(ctx, "bad2", domain.GroupVisibilityPublic, nil,
		[]domain.ProtocolConvert{domain.ProtocolConvert("bogus")})
	require.ErrorIs(t, err, ErrInvalidInput)

	// 重复方向 → 400
	_, err = svc.CreateGroup(ctx, "dup", domain.GroupVisibilityPublic, nil,
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertChatToResp})
	require.ErrorIs(t, err, ErrInvalidInput, "重复方向 → 400")

	// 同客户端格式多方向（chat_to_resp + chat_to_mess）→ 400（语义歧义）
	_, err = svc.CreateGroup(ctx, "clash", domain.GroupVisibilityPublic, nil,
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertChatToMess})
	require.ErrorIs(t, err, ErrInvalidInput, "同客户端格式多方向 → 400")
	// 不同客户端格式多方向可并存
	_, err = svc.CreateGroup(ctx, "ok-mix", domain.GroupVisibilityPublic, nil,
		[]domain.ProtocolConvert{domain.ProtocolConvertRespToMess, domain.ProtocolConvertChatToResp})
	require.NoError(t, err, "不同客户端格式多方向合法")

	// Update 非法值 → 400
	g2, err := svc.CreateGroup(ctx, "g-upd", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	g2.ProtocolConverts = []domain.ProtocolConvert{domain.ProtocolConvert("bogus")}
	_, err = svc.UpdateGroup(ctx, g2)
	require.ErrorIs(t, err, ErrInvalidInput)

	// Update 同客户端格式冲突 → 400
	g2.ProtocolConverts = []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertChatToMess}
	_, err = svc.UpdateGroup(ctx, g2)
	require.ErrorIs(t, err, ErrInvalidInput)

	// Update 合法值 → 生效
	g2.ProtocolConverts = []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}
	updated, err := svc.UpdateGroup(ctx, g2)
	require.NoError(t, err)
	require.Equal(t, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, updated.ProtocolConverts)

	// Update 显式空数组 = 清空既有方向（off）
	g2.ProtocolConverts = []domain.ProtocolConvert{}
	updated2, err := svc.UpdateGroup(ctx, g2)
	require.NoError(t, err)
	require.Empty(t, updated2.ProtocolConverts, "显式空数组 → 清空")
}

// --- helpers ---

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

// seedAccount 建账号（ext 测试用；api_key 静态 key 语义）。
func seedExtAccount(t *testing.T, svc *Service, tplID int64) *domain.Account {
	t.Helper()
	a, err := svc.CreateAccount(context.Background(), &domain.Account{
		Name: "a", TemplateID: tplID, UpstreamKey: "sk-a", Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	return a
}
