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

// seedPrivateProxy 在供应商作用域下建**私池**代理（§3.6）：地址用公网域名——供应商
// 建私网出口会被同一条 spec 的判据拒掉（那两个判据互不干扰，各自的用例各自覆盖）。
func seedPrivateProxy(t *testing.T, svc *Service, uid int64, name string) int64 {
	t.Helper()
	in := validProxyInput()
	in.Name = name
	in.Host = "proxy-" + name + ".example.com"
	id, err := svc.CreateProxy(supplierScopeCtx(uid), in)
	require.NoError(t, err)
	return id
}

// TestProxyBindingPredicate spec §3.6 的**唯一绑定谓词**四象限：
//
//	平台代理（owner NULL）  ⇒ 仅管理员可分配；可分配给平台账号或供应商账号
//	私池代理（owner = X）   ⇒ 只能承载 X 自己的账号（管理员也遵守）
//	平台账号（owner = 0）   ⇒ 谓词最后一行恒假 ⇒ 绑不上任何私池代理
//
// 越界一律 **404**（不是 403）：不泄露「别人有这条代理」。三个写面（创建/单改/批量）
// 共用同一个谓词，故每个面都抽一条越界样本。
func TestProxyBindingPredicate(t *testing.T) {
	svc, _, _ := newProxyTestService()
	adminCtx := context.Background()
	const (
		supplierA = int64(101)
		supplierB = int64(202)
	)
	ctxA := supplierScopeCtx(supplierA)
	tpl := seedTemplate(t, svc, "tpl-proxy-pred")

	platformProxy := seedBindingProxy(t, svc, "pred-platform")
	privateA := seedPrivateProxy(t, svc, supplierA, "pred-private-a")

	newAccount := func(name string, owner int64) int64 {
		p := repository.AccountPatch{Name: &name, TemplateID: &tpl.ID, UpstreamKey: new("k-" + name)}
		if owner > 0 {
			p.SupplierUserID = &owner
		}
		acc, err := svc.CreateAccount(adminCtx, p)
		require.NoError(t, err)
		return acc.ID
	}
	platformAcc := newAccount("pred-platform-acc", 0)
	accA := newAccount("pred-a-acc", supplierA)
	accB := newAccount("pred-b-acc", supplierB)

	bind := func(ctx context.Context, accID, proxyID int64) error {
		_, err := svc.PatchAccount(ctx, accID, repository.AccountPatch{ProxyID: &proxyID}, nil)
		return err
	}

	// ① 平台代理 × 平台账号（管理员）⇒ ✅
	require.NoError(t, bind(adminCtx, platformAcc, platformProxy))
	// ② 平台代理 × 供应商账号（管理员）⇒ ✅（平台代理可分配给任何账号）
	require.NoError(t, bind(adminCtx, accA, platformProxy))
	// ③ 平台代理 × 供应商账号，但调用方是供应商自己 ⇒ 404（平台池不是公共池：
	//    供应商看不见也绑不上，哪怕这条已经由管理员分配给过他）
	require.ErrorIs(t, bind(ctxA, accA, platformProxy), ErrNotFound)
	// ④ 私池 A × A 的账号（管理员）⇒ ✅
	require.NoError(t, bind(adminCtx, accA, privateA))
	// ⑤ 私池 A × A 的账号（A 自己）⇒ ✅
	require.NoError(t, bind(ctxA, accA, privateA))
	// ⑥ 私池 A × B 的账号（管理员）⇒ 404：私池只能承载其主的流量
	require.ErrorIs(t, bind(adminCtx, accB, privateA), ErrNotFound)
	require.Nil(t, accProxyID(t, svc, accB), "404 路径不得留下半套写入")
	// ⑦ 私池 A × 平台账号（管理员）⇒ 404（平台账号 owner=0 恒不匹配）
	require.ErrorIs(t, bind(adminCtx, platformAcc, privateA), ErrNotFound)
	kept, err := svc.GetAccount(adminCtx, platformAcc)
	require.NoError(t, err)
	require.NotNil(t, kept.ProxyID)
	require.Equal(t, platformProxy, *kept.ProxyID, "拒绝路径保持原绑定")

	// ⑧ 同一补丁「转属给 A + 绑 A 的私池」⇒ 通过：谓词判的是**落库后**的归属
	//    （否则会拒绝一个最终状态完全合法的写入）
	ownerA := supplierA
	_, err = svc.PatchAccount(adminCtx, accB, repository.AccountPatch{SupplierUserID: &ownerA, ProxyID: &privateA}, nil)
	require.NoError(t, err, "转属与换出口同批落库 ⇒ 判写完那一行")

	// ⑨ 创建面：私池 A 绑到 B 的新账号 ⇒ 404，且不建出账号
	badName := "pred-create-bad"
	_, err = svc.CreateAccount(adminCtx, repository.AccountPatch{
		Name: &badName, TemplateID: &tpl.ID, UpstreamKey: new("k-bad"),
		SupplierUserID: &[]int64{supplierB}[0], ProxyID: &privateA,
	})
	require.ErrorIs(t, err, ErrNotFound, "创建面共用同一谓词")
	list, _, err := svc.ListAccounts(adminCtx, repository.ListQuery{})
	require.NoError(t, err)
	for _, acc := range list {
		require.NotEqual(t, badName, acc.Name, "404 路径不得建出账号")
	}

	// ⑩ 软删的代理 ⇒ 404（不存在与越界同码）
	goneProxy := seedPrivateProxy(t, svc, supplierA, "pred-private-gone")
	require.NoError(t, svc.DeleteProxy(ctxA, goneProxy))
	require.ErrorIs(t, bind(ctxA, accA, goneProxy), ErrNotFound, "已软删 ⇒ 404")

	// ⑪ 批量面**逐账号**判定：一真一假 ⇒ 整批 404，原绑定不变
	accA2 := newAccount("pred-a2-acc", supplierA)
	accB2 := newAccount("pred-b2-acc", supplierB)
	require.NoError(t, bind(adminCtx, accA2, platformProxy))
	require.NoError(t, bind(adminCtx, accB2, platformProxy))
	_, err = svc.UpdateAccountsBatch(adminCtx, []int64{accA2, accB2}, repository.AccountPatch{ProxyID: &privateA})
	require.ErrorIs(t, err, ErrNotFound, "任一账号不命中谓词 ⇒ 整批 404")
	for _, id := range []int64{accA2, accB2} {
		acc, gErr := svc.GetAccount(adminCtx, id)
		require.NoError(t, gErr)
		require.NotNil(t, acc.ProxyID)
		require.Equal(t, platformProxy, *acc.ProxyID, "整批失败不改变任何原绑定")
	}
	// 全真才落：两个都属于 A ⇒ 通过
	_, err = svc.UpdateAccountsBatch(adminCtx, []int64{accA, accA2}, repository.AccountPatch{ProxyID: &privateA})
	require.NoError(t, err)
}

// accProxyID 单条账号当前绑定（nil = 直连）。
func accProxyID(t *testing.T, svc *Service, id int64) *int64 {
	t.Helper()
	acc, err := svc.GetAccount(context.Background(), id)
	require.NoError(t, err)
	return acc.ProxyID
}

// TestTransferKeepsBindingGuard 转属时的**既存绑定**守卫（spec §3.6 的第二个入口）。
//
// 谓词只在「显式给了 proxy_id」时执行，于是有一个漏洞形状：补丁只把账号从供应商 A
// 转给 B（不碰 proxy_id），而该账号当前绑的是 A 的私池代理 —— 写完之后那一行落在谓词
// 之外：B 的流量走 A 的出口。本门钉住处置：**400，要求调用方显式决定**（同一补丁里
// 解绑或改绑新归属自己的代理），而不是静默解绑（隐式改掉出栈路径）或静默放过。
func TestTransferKeepsBindingGuard(t *testing.T) {
	svc, _, _ := newProxyTestService()
	adminCtx := context.Background()
	// 用变量而不是常量：补丁里的归属字段是 *int64，需要对它们取地址。
	var (
		supplierA = int64(303)
		supplierB = int64(404)
	)
	tpl := seedTemplate(t, svc, "tpl-xfer")
	privateA := seedPrivateProxy(t, svc, supplierA, "xfer-private-a")
	privateB := seedPrivateProxy(t, svc, supplierB, "xfer-private-b")
	platformProxy := seedBindingProxy(t, svc, "xfer-platform")

	newAccount := func(name string, owner int64) int64 {
		p := repository.AccountPatch{Name: &name, TemplateID: &tpl.ID, UpstreamKey: new("k-" + name)}
		if owner > 0 {
			p.SupplierUserID = &owner
		}
		acc, err := svc.CreateAccount(adminCtx, p)
		require.NoError(t, err)
		return acc.ID
	}
	bind := func(accID, proxyID int64) {
		t.Helper()
		_, err := svc.PatchAccount(adminCtx, accID, repository.AccountPatch{ProxyID: &proxyID}, nil)
		require.NoError(t, err)
	}
	ownerOf := func(id int64) int64 {
		t.Helper()
		acc, err := svc.GetAccount(adminCtx, id)
		require.NoError(t, err)
		return acc.SupplierUserID
	}

	// ① 只转属、当前绑着 A 的私池代理 ⇒ 400，且归属与绑定都不动（不留半套写入）
	acc := newAccount("xfer-acc", supplierA)
	bind(acc, privateA)
	_, err := svc.PatchAccount(adminCtx, acc, repository.AccountPatch{SupplierUserID: &supplierB}, nil)
	require.ErrorIs(t, err, ErrInvalidInput, "只转属会把 A 的出口留给 B ⇒ 400")
	require.Equal(t, supplierA, ownerOf(acc), "失败路径不得改归属")
	require.Equal(t, privateA, *accProxyID(t, svc, acc), "失败路径不得动绑定")

	// ② 同一补丁「转属 + 解绑」⇒ 成功（显式决定是这条路的唯一入口）
	_, err = svc.PatchAccount(adminCtx, acc, repository.AccountPatch{
		SupplierUserID: &supplierB, ProxyID: new(int64(0)),
	}, nil)
	require.NoError(t, err)
	require.Equal(t, supplierB, ownerOf(acc))
	require.Nil(t, accProxyID(t, svc, acc), "解绑后回到直连")

	// ③ 同一补丁「转属 + 改绑新归属自己的代理」⇒ 成功
	acc2 := newAccount("xfer-acc2", supplierA)
	bind(acc2, privateA)
	_, err = svc.PatchAccount(adminCtx, acc2, repository.AccountPatch{
		SupplierUserID: &supplierB, ProxyID: &privateB,
	}, nil)
	require.NoError(t, err)
	require.Equal(t, supplierB, ownerOf(acc2))
	require.Equal(t, privateB, *accProxyID(t, svc, acc2))

	// ④ 平台代理绑定 + 只转属 ⇒ 成功：平台代理由管理员维护，转属后仍合规
	acc3 := newAccount("xfer-acc3", supplierA)
	bind(acc3, platformProxy)
	_, err = svc.PatchAccount(adminCtx, acc3, repository.AccountPatch{SupplierUserID: &supplierB}, nil)
	require.NoError(t, err)
	require.Equal(t, supplierB, ownerOf(acc3))
	require.Equal(t, platformProxy, *accProxyID(t, svc, acc3), "平台代理不受转属限制")

	// ⑤ 批量面：一批两个账号、其中一个绑着 A 的私池 ⇒ 整批 400，两侧原绑定不变
	ok1 := newAccount("xfer-ok1", supplierA)
	bad := newAccount("xfer-bad", supplierA)
	bind(ok1, privateA)
	bind(bad, privateA)
	_, err = svc.UpdateAccountsBatch(adminCtx, []int64{ok1, bad}, repository.AccountPatch{SupplierUserID: &supplierB})
	require.ErrorIs(t, err, ErrInvalidInput, "批量转属同样要逐账号判定")
	for _, id := range []int64{ok1, bad} {
		require.Equal(t, supplierA, ownerOf(id), "整批失败不改归属")
		require.Equal(t, privateA, *accProxyID(t, svc, id), "整批失败不改绑定")
	}
	// 同一批「转属 + 解绑」⇒ 通过
	_, err = svc.UpdateAccountsBatch(adminCtx, []int64{ok1, bad}, repository.AccountPatch{
		SupplierUserID: &supplierB, ProxyID: new(int64(0)),
	})
	require.NoError(t, err)
}
