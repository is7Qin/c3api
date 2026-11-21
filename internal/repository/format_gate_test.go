// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/stretchr/testify/require"
)

// TestCreateTemplateRejectsNonResponsesBPSFormats 仓库写面上 BPS 格式门的唯一证据。
//
// 断言落在 CreateTemplate 而不是纯函数：validateBPSTemplateFormats 只被写面调用
// （CreateTemplate / UpdateTemplate / 批量导入），单测纯函数只能证明「函数会拒」，
// 证明不了「写面会拒」——把调用点删掉、函数留着也能全绿。
//
// 三条信号，缺一不可：
//   - 拒绝行：BPS 模板声明非 responses 格式 → 写面拒绝（M3「门恒 true」在此红）；
//   - 无残留行：拒绝发生在入库之前，两次拒绝后模板表必须仍是空的；
//   - 对照行：responses-ws 能落库并原样回读——没有对照，一个「见 BPS 就拒」的门
//     也能让上面两段绿；同时它是 BPS 行「缩行」方向在本层的唯一信号。
//
// 其余类型带 anthropic 仍放行是本门的**作用范围**断言（只做 BPS 子集视图，不改成
// 全量白名单），与格式矩阵的行内容无关——那一份在 domain 的 TestFormatPolicyMatrix。
func TestCreateTemplateRejectsNonResponsesBPSFormats(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		formats []domain.RequestFormat
	}{
		{"bps-anthropic", []domain.RequestFormat{domain.FormatAnthropic}},
		{"bps-chat", []domain.RequestFormat{domain.FormatOpenAIChat}},
	} {
		_, err := repos.CreateTemplate(ctx, &domain.Template{
			Name:             tc.name,
			CredentialType:   credential.TypeBPS,
			SupportedFormats: tc.formats,
		})
		require.ErrorIs(t, err, repository.ErrInvalidInput,
			"%s：BPS 模板声明非 responses 格式必须被写面拒绝", tc.name)
	}

	rows, total, err := repos.ListTemplates(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, total, "拒绝必须发生在入库之前：模板表不得留下行")
	require.Empty(t, rows)

	created, err := repos.CreateTemplate(ctx, &domain.Template{
		Name:             "bps-responses-ws",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err, "对照行：BPS + responses-ws 必须能落库")
	back, err := repos.GetTemplate(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, back.SupportedFormats,
		"对照行：responses-ws 必须原样回读")

	for _, typ := range []credential.Type{credential.TypeAPIKey, credential.TypeCodexOAuth} {
		_, err := repos.CreateTemplate(ctx, &domain.Template{
			Name:             "scope-" + string(typ),
			CredentialType:   typ,
			SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic},
			ModelMapping:     domain.ModelMapping{},
		})
		require.NoError(t, err, "%q 不在本门收紧范围：带 anthropic 的模板必须仍可写", typ)
	}
}
