// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGBatchProxyIDTriState 钉住 AccountPatch.ProxyID 的三态落值（spec §3.5 第 6
// 条 / §6.2 第 3 条）：nil = 不变；&0 = 解绑（落 NULL）；&id>0 = 绑定。三者必须
// 在**同一个批量入口**上语义分明——否则 API 的「缺省」与「0」无法区分。
func TestPGBatchProxyIDTriState(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-tristate")

	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "tri", TemplateID: tpl.ID, UpstreamKey: "sk-tri",
		MaxConcurrency: 8, Enabled: true,
	})
	require.NoError(t, err)
	require.Nil(t, acc.ProxyID, "新账号默认直连（NULL）")

	proxyA := seedPGProxyNamed(t, repos, "tristate-a")

	// ① &id>0 = 绑定。
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &proxyA})
	require.NoError(t, err)
	got, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ProxyID)
	require.Equal(t, proxyA, *got.ProxyID)

	// ② nil = 不变（其它字段照常写入，绑定不动）。
	renamed := "tri-renamed"
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Name: &renamed})
	require.NoError(t, err)
	got, err = repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, renamed, got.Name)
	require.NotNil(t, got.ProxyID)
	require.Equal(t, proxyA, *got.ProxyID, "补丁缺省 proxy_id ⇒ 绑定不变")

	// ③ &0 = 解绑（落 NULL = 直连）。
	unbind := int64(0)
	results, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &unbind})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, []domain.AccountField{domain.FieldProxyID}, results[0].ChangedFields.Fields(),
		"解绑是真实变更（旧值 = 具体代理，目标 = NULL）")
	got, err = repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, got.ProxyID, "&0 ⇒ proxy_id 落 NULL")

	// ④ 幂等重写：已是直连时再传 &0 ⇒ 无变更位（不推进 K）。
	results, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &unbind})
	require.NoError(t, err)
	require.Empty(t, results[0].ChangedFields.Fields(), "已是直连时再解绑不算变更")

	// ⑤ 绑定后同值重写 ⇒ 无变更位。
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &proxyA})
	require.NoError(t, err)
	results, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &proxyA})
	require.NoError(t, err)
	require.Empty(t, results[0].ChangedFields.Fields(), "同值重写不算变更")
}

// TestPGProxyIDIsIdentityChange 钉住 proxy_id 的**身份语义**（spec §3.4 最后一段）：
// 绑定与解绑都是真实变更，变更集恰含 FieldProxyID，IdentityChanged() 为真，且
// 身份代际 K 真的 +1（配置代际 C 每次都 +1，不用于判据）。
func TestPGProxyIDIsIdentityChange(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-identity")

	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "ident", TemplateID: tpl.ID, UpstreamKey: "sk-ident",
		MaxConcurrency: 8, Enabled: true,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), acc.IdentityRevision)

	proxyA := seedPGProxyNamed(t, repos, "identity-a")
	proxyB := seedPGProxyNamed(t, repos, "identity-b")

	// 绑定：变更位 = {proxy_id}，K 1 → 2。
	results, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &proxyA})
	require.NoError(t, err)
	require.Equal(t, []domain.AccountField{domain.FieldProxyID}, results[0].ChangedFields.Fields())
	require.True(t, results[0].ChangedFields.IdentityChanged(), "换出口即换身份（出口 IP 是身份第三维）")
	bound, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), bound.IdentityRevision, "绑定代理推进 K")
	require.Equal(t, int64(2), bound.LifecycleRevision, "配置代际 C 照常推进")

	// 换绑到另一个代理：同样推进 K。
	results, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &proxyB})
	require.NoError(t, err)
	require.Equal(t, []domain.AccountField{domain.FieldProxyID}, results[0].ChangedFields.Fields())
	require.True(t, results[0].ChangedFields.IdentityChanged())
	swapped, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(3), swapped.IdentityRevision, "换绑出口推进 K（在途工件必须作废）")
	require.Equal(t, proxyB, *swapped.ProxyID)

	// 解绑：同样推进 K。
	unbind := int64(0)
	results, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &unbind})
	require.NoError(t, err)
	require.True(t, results[0].ChangedFields.IdentityChanged(), "解绑回直连同样换了上游看到的出口")
	direct, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(4), direct.IdentityRevision, "解绑推进 K")
	require.Nil(t, direct.ProxyID)

	// 幂等重写（&0 on NULL）：C +1，K 不动。
	results, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &unbind})
	require.NoError(t, err)
	require.False(t, results[0].ChangedFields.IdentityChanged())
	idem, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(4), idem.IdentityRevision, "幂等解绑不推进 K")
	require.Equal(t, int64(5), idem.LifecycleRevision, "幂等重写仍推进 C")
}
