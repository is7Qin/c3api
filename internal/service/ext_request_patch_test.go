// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// mustRequestPatch 走唯一 wire 入口构造领域对象（service 测试不手搓形状）。
func mustRequestPatch(t *testing.T, raw string) *domain.RequestPatch {
	t.Helper()
	p, err := domain.ParseRequestPatch(json.RawMessage(raw))
	require.NoError(t, err)
	require.NotNil(t, p)
	return p
}

// TestUpsertTemplateExtRequestPatchCustomOnly 模板 ext 的 request_patch 策略面
// （spec custom-request-patch §2.6/§5.2）：
//   - custom 模板可写、可清空（空 patch = 全列替换的清空语义）；
//   - 非 custom（responses-special / codex-oauth / codex-pat / bps）非空 ⇒ 400；
//   - 400 先于既有 409 降级门（request_patch 的 custom-only 判定与
//     upstream_paths 并列，都在守卫写之前），顺序本身是契约。
func TestUpsertTemplateExtRequestPatchCustomOnly(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	patch := mustRequestPatch(t, `{"headers":{"User-Agent":"vibe/1"},"formats":{"openai-chat":{"set":{"metadata.call_source":"x"},"delete":["metadata.client_field"]}}}`)

	custom := seedExtTemplate(t, svc, "t-custom", credential.TypeCustom, domain.FormatOpenAIChat)
	saved, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: custom.ID, CredentialType: credential.TypeCustom, RequestPatch: patch,
	}, "")
	require.NoError(t, err, "custom 模板可写 request_patch")
	require.NotNil(t, saved.RequestPatch)
	require.Equal(t, "vibe/1", saved.RequestPatch.Headers["User-Agent"])

	// 清空：空 patch 允许（PUT 全列替换语义；web 回显链路漏字段时会走到这条）。
	saved, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: custom.ID, CredentialType: credential.TypeCustom,
	}, "")
	require.NoError(t, err)
	require.Nil(t, saved.RequestPatch)

	// 非 custom 模板非空 ⇒ 400（包括陈旧 ext 行场景：写面按 ext 行类型判定，
	// 而 ext 行类型必须 == 父模板类型，二者等价）。
	for _, ct := range []credential.Type{
		credential.TypeResponsesSpecial, credential.TypeCodexOAuth, credential.TypeCodexPAT, credential.TypeBPS,
	} {
		tpl := seedExtTemplate(t, svc, "t-"+string(ct), ct, domain.FormatOpenAIResponses)
		_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: ct, RequestPatch: patch,
		}, "")
		require.ErrorIs(t, err, ErrInvalidInput, "type %s 非空 request_patch 必须 400", ct)
	}

	// 顺序契约：codex 模板旧值 ws + 本次降级到 http + 非空 request_patch，
	// 无 confirm ⇒ 必须先 400（策略面）而不是 409（降级门）。
	codex := seedExtTemplate(t, svc, "t-codex-ws", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	_, _, err = svc.store.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: codex.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, func(*domain.TemplateExt) (bool, error) { return false, nil })
	require.NoError(t, err)
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: codex.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
		RequestPatch:            patch,
	}, "")
	require.ErrorIs(t, err, ErrInvalidInput, "request_patch 策略面 400 必须先于降级门 409: %v", err)
	require.NotErrorIs(t, err, ErrConflict)
}
