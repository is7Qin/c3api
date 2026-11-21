// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// bpsShapedFormats 是 BPS 模板唯一合法的格式集合；对照组的 api_key 模板刻意沿用
// 同一行，让「凭据类型」成为两组之间唯一的变量。
func bpsShapedFormats() []domain.RequestFormat {
	return []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS}
}

// TestCreateAccountRejectsBPSTemplate 管理面的 BPS 创建围栏。
//
// 为什么必须拒绝：BPS 凭据（installation_id + profile）只活在 account_ext 里，创建
// 走的是设备登录流程。若这条围栏缺失，管理面能用 BPS 模板建出一个没有 account_ext
// 的账号；它进快照后请求期取凭据必然失败（sdkbridge/bps.go 的 ErrUnsupported 分支），
// 而账号本身看起来是健康的——于是该账号与同组路由永久失败且无自愈路径。
//
// 围栏位于落库之前，因此副作用必须为零：失败后账号列表仍为空。
func TestCreateAccountRejectsBPSTemplate(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newProxyTestService()

	tpl, err := svc.CreateTemplate(ctx, &domain.Template{
		Name:             "bps",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: bpsShapedFormats(),
	})
	require.NoError(t, err)

	name := "bps-acc"
	_, err = svc.CreateAccount(ctx, repository.AccountPatch{Name: &name, TemplateID: &tpl.ID})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Contains(t, err.Error(), "use device login", "文案要点明唯一合法入口，否则调用方无从修复")

	accs, total, err := svc.ListAccounts(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, total, "围栏必须在落库之前，失败不得留下账号")
	require.Empty(t, accs)
}

// TestCreateAccountAcceptsAPIKeyTemplate 是上一条的对照组，且必须是独立测试：
// require 失败即终止当前测试，写在负断言之后对照就永远不可证——围栏缺失时我们既要
// 看到 BPS 那条红，也要看到这条仍绿（证明 Create 没有整体坏掉）。
func TestCreateAccountAcceptsAPIKeyTemplate(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newProxyTestService()

	tpl, err := svc.CreateTemplate(ctx, &domain.Template{
		Name:             "api-key",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: bpsShapedFormats(),
	})
	require.NoError(t, err)

	name := "static-acc"
	key := "sk-test"
	acc, err := svc.CreateAccount(ctx, repository.AccountPatch{Name: &name, TemplateID: &tpl.ID, UpstreamKey: &key})
	require.NoError(t, err)
	require.Equal(t, key, acc.UpstreamKey)
	require.Equal(t, tpl.ID, acc.TemplateID)

	accs, total, err := svc.ListAccounts(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, accs, 1)
}
