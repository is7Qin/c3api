// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

// seedBindingProxy 建一个启用的代理条目并返回其 id（名字唯一 ⇒ 各用例自带名字）。
func seedBindingProxy(t *testing.T, svc *Service, name string) int64 {
	t.Helper()
	in := validProxyInput()
	in.Name = name
	id, err := svc.CreateProxy(context.Background(), in)
	require.NoError(t, err)
	return id
}

// TestPatchAccountProxyBinding 绑定三态（spec §3.5 第 6 条 / §9.1 绑定校验门）：
//
//	&id>0 → 必须存在且未软删（否则 404），绑成功后回读相等
//	&0    → 解绑（回落直连，无需目标存在）
//	nil   → 不变（不校验）
//	&id<0 → 400（值域）
func TestPatchAccountProxyBinding(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "tpl-proxy-bind")
	acc := seedAccount(t, svc, tpl.ID, "acc-proxy-bind")

	// 不存在的 id → 404（且不落库）。
	missing := int64(999)
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &missing}, nil)
	require.ErrorIs(t, err, ErrNotFound, "绑定不存在的代理 → 404")
	require.Contains(t, err.Error(), "id=999", "404 带缺失 id 详情")
	unchanged, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, unchanged.ProxyID, "404 路径不得留下半套写入")

	// 已软删的 id → 404（GetProxy 把软删视同不存在）。
	softDeleted := seedBindingProxy(t, svc, "soft-deleted")
	require.NoError(t, svc.DeleteProxy(ctx, softDeleted))
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &softDeleted}, nil)
	require.ErrorIs(t, err, ErrNotFound, "绑定已软删的代理 → 404")

	// 负数 → 400（值域校验先于存在性）。
	negative := int64(-1)
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &negative}, nil)
	require.ErrorIs(t, err, ErrInvalidInput, "proxy_id < 0 → 400")

	// 绑定成功 → 回读相等。
	live := seedBindingProxy(t, svc, "live")
	patched, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &live}, nil)
	require.NoError(t, err)
	require.NotNil(t, patched.ProxyID)
	require.Equal(t, live, *patched.ProxyID)
	readBack, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, readBack.ProxyID)
	require.Equal(t, live, *readBack.ProxyID, "GetAccount 的 ProxyID 回读相等")

	// &0 = 解绑。
	unbind := int64(0)
	patched, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &unbind}, nil)
	require.NoError(t, err)
	require.Nil(t, patched.ProxyID, "&0 ⇒ 解绑（落 NULL = 直连）")
}

// TestPatchAccountProxyBindingIdempotent 幂等绑定（同 id 再写一次）不报错，且
// IdentityChanged() 为假——K 不重复推进（代理是身份类字段：换出口才换身份）。
func TestPatchAccountProxyBindingIdempotent(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "tpl-proxy-idem")
	acc := seedAccount(t, svc, tpl.ID, "acc-proxy-idem")
	proxyID := seedBindingProxy(t, svc, "idem")

	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &proxyID}, nil)
	require.NoError(t, err)
	afterBind, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)

	// 同 id 再绑：不报错，且 K 不再推进（值没变 ⇒ 不是身份变更）。
	results, err := svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &proxyID})
	require.NoError(t, err, "幂等绑定不报错")
	require.Len(t, results, 1)
	require.False(t, results[0].ChangedFields.IdentityChanged(),
		"同 id 重绑不算身份变更（幂等重写不推进 K）")
	afterRebind, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, afterBind.IdentityRevision, afterRebind.IdentityRevision,
		"同值重绑不推进 IdentityRevision")

	// 换来路明确的「换出口」才算身份变更（对照，防止上面因字段没接上而假绿）。
	other := seedBindingProxy(t, svc, "idem-other")
	results, err = svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{ProxyID: &other})
	require.NoError(t, err)
	require.True(t, results[0].ChangedFields.IdentityChanged(), "换出口 = 换身份（Identity=true）")
	afterSwitch, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Greater(t, afterSwitch.IdentityRevision, afterBind.IdentityRevision, "换出口推进 K")
}

// TestCreateAccountProxyBinding 创建面同样校验并落列：>0 必须存在（否则 404），
// 成功则 ProxyID 落库可回读；批量面缺 id 整批 404。
func TestCreateAccountProxyBinding(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "tpl-proxy-create")

	missing := int64(999)
	name := "acc-proxy-create-bad"
	_, err := svc.CreateAccount(ctx, repository.AccountPatch{
		Name: &name, TemplateID: &tpl.ID, UpstreamKey: new("sk-bad"), ProxyID: &missing,
	})
	require.ErrorIs(t, err, ErrNotFound, "创建面绑定不存在的代理 → 404")
	list, _, err := svc.ListAccounts(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Empty(t, list, "404 路径不得建出账号")

	live := seedBindingProxy(t, svc, "create-live")
	name = "acc-proxy-create-ok"
	created, err := svc.CreateAccount(ctx, repository.AccountPatch{
		Name: &name, TemplateID: &tpl.ID, UpstreamKey: new("sk-ok"), ProxyID: &live,
	})
	require.NoError(t, err)
	require.NotNil(t, created.ProxyID)
	require.Equal(t, live, *created.ProxyID, "创建即落 proxy_id")
	readBack, err := svc.GetAccount(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, readBack.ProxyID)
	require.Equal(t, live, *readBack.ProxyID)

	// 批量面：缺 id → 整批 404（与组/模板的批量前置校验同语义）。
	_, err = svc.UpdateAccountsBatch(ctx, []int64{created.ID}, repository.AccountPatch{ProxyID: &missing})
	require.ErrorIs(t, err, ErrNotFound, "批量面绑定不存在的代理 → 整批 404")
	still, err := svc.GetAccount(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, still.ProxyID)
	require.Equal(t, live, *still.ProxyID, "整批失败不改变原绑定")
}

// TestAccountPatchProxyIDOnlyIsNotEmpty 只有 proxy_id 的补丁**不是**空补丁（否则
// 「直连↔代理」这一个动作会被 400 挡在门外）。
func TestAccountPatchProxyIDOnlyIsNotEmpty(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()

	id := int64(0)
	require.NoError(t, validateAccountPatch(repository.AccountPatch{ProxyID: &id}),
		"仅 proxy_id 的补丁是合法写")

	tpl := seedTemplate(t, svc, "tpl-proxy-only")
	acc := seedAccount(t, svc, tpl.ID, "acc-proxy-only")
	patched, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &id}, nil)
	require.NoError(t, err)
	require.Nil(t, patched.ProxyID)

	// 空补丁仍然拒绝（无字段 = 无意义写）。
	require.ErrorIs(t, validateAccountPatch(repository.AccountPatch{}), ErrInvalidInput)
}
