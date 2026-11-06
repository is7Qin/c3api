// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// seedPGProxyNamed 建一个启用的代理条目并返回其 id（名字唯一 ⇒ 每个子用例用自己的
// 名字）。代理表无外键依赖，可在任何种子之前创建。
func seedPGProxyNamed(t *testing.T, repos *repository.Repository, name string) int64 {
	t.Helper()
	id, err := repos.Proxies.CreateProxy(context.Background(), domain.Proxy{
		Name: name, Scheme: domain.ProxySchemeSOCKS5, Host: "127.0.0.1", Port: 1080,
		Enabled: true,
	})
	require.NoError(t, err)
	require.NotZero(t, id)
	return id
}

// TestPGProxyCRUD 覆盖代理条目的基本读写语义：创建落全字段、列表按 id 稳定序、
// GET 回读、全量更新（含可空列清空）、软删后列表不含且 GET 视同不存在（重复删除
// 也返回 NotFound，不静默成功）。
func TestPGProxyCRUD(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	id, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "crud-1", Scheme: domain.ProxySchemeHTTPS, Host: "10.0.0.7", Port: 3128,
		Username: "u1", Password: "p1", Enabled: true, Remark: "primary",
	})
	require.NoError(t, err)
	otherID, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "crud-2", Scheme: domain.ProxySchemeSOCKS5H, Host: "10.0.0.8", Port: 1080,
		Enabled: false,
	})
	require.NoError(t, err)
	require.Greater(t, otherID, id, "id 递增（列表 ORDER BY id 的稳定性前提）")

	got, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, id, got.ID)
	require.Equal(t, "crud-1", got.Name)
	require.Equal(t, domain.ProxySchemeHTTPS, got.Scheme)
	require.Equal(t, "10.0.0.7", got.Host)
	require.Equal(t, 3128, got.Port)
	require.Equal(t, "u1", got.Username)
	require.Equal(t, "p1", got.Password, "密码是明文列（§3.3），仓库层原样读回")
	require.True(t, got.Enabled)
	require.Equal(t, "primary", got.Remark)
	require.Nil(t, got.LastProbeAt, "从未探测 = nil")
	require.False(t, got.LastProbeOK)
	require.Empty(t, got.LastProbeError)

	list, err := repos.Proxies.ListProxies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, id, list[0].ID)
	require.Equal(t, otherID, list[1].ID)
	require.False(t, list[1].Enabled)
	require.Empty(t, list[1].Username, "无认证条目：可空列落 NULL → 零值")

	// 全量更新：改名 + 禁用 + 清空认证与备注（空串归一 NULL）。
	got.Name = "crud-1-renamed"
	got.Scheme = domain.ProxySchemeHTTP
	got.Host = "proxy.example.com"
	got.Port = 8080
	got.Remark = ""
	got.Enabled = false
	// 凭据三态：两列各给 &"" 才清空（本轮要清掉 u1/p1 的认证）。凭据不走
	// domain.Proxy 的字段（更新面只有 ProxyCredential 能设凭据）。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, got, repository.ProxyCredential{
		Username: new(""), Password: new(""),
	}))

	updated, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "crud-1-renamed", updated.Name)
	require.Equal(t, domain.ProxySchemeHTTP, updated.Scheme)
	require.Equal(t, "proxy.example.com", updated.Host)
	require.Equal(t, 8080, updated.Port)
	require.False(t, updated.Enabled)
	require.Empty(t, updated.Username)
	require.Empty(t, updated.Password)
	require.Empty(t, updated.Remark)

	// 更新幂等：同值再写一遍不报错、结果不变（password=nil = 不变）。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, updated, repository.ProxyCredential{}))
	again, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, updated.Name, again.Name)
	require.Equal(t, updated.Port, again.Port)

	// 软删：列表不含、GET 不存在、重复删除也 NotFound。
	require.NoError(t, repos.Proxies.DeleteProxy(ctx, id))
	list, err = repos.Proxies.ListProxies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, otherID, list[0].ID)
	_, err = repos.Proxies.GetProxy(ctx, id)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.ErrorIs(t, repos.Proxies.DeleteProxy(ctx, id), repository.ErrNotFound)
	// 软删行不可更新。
	require.ErrorIs(t, repos.Proxies.UpdateProxy(ctx, updated, repository.ProxyCredential{}), repository.ErrNotFound)
}

// TestPGProxyNameUnique 钉住 name 唯一的**每归属**语义（spec-proxy-pool §2.5）：
// 平台池（`owner_uid IS NULL`）内互不同名；私池同一供应商内互不同名，**不同供应商
// 可以同名**（否则 B 建不了 A 已用的名字，且一次尝试就构成「猜别人有没有这条代理」
// 的探测面）。软删语义不变：两个部分唯一索引的谓词都不含 deleted_at ⇒ 删条目不释放
// 名字（与 template 同语义）。
func TestPGProxyNameUnique(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	// 两个供应商的作用域 ctx（同一 uid = 同租户；不同 uid = 跨租户）。
	ownerA := domain.WithAccountScope(ctx, domain.SupplierAccountScope(9001))
	ownerB := domain.WithAccountScope(ctx, domain.SupplierAccountScope(9002))

	// ---- 平台池（无 scope ⇒ owner_uid IS NULL）：彼此仍唯一 ----
	id, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeSOCKS5, Host: "h1", Port: 1080, Enabled: true,
	})
	require.NoError(t, err)

	_, err = repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeHTTP, Host: "h2", Port: 8080, Enabled: true,
	})
	require.ErrorIs(t, err, repository.ErrConflict, "平台池内重复 name → ErrConflict（service 映射 409）")
	require.Contains(t, err.Error(), `name="dup"`, "冲突详情含 name")

	// ---- 跨归属同名：允许（本阶段的核心改动）----
	_, err = repos.Proxies.CreateProxy(ownerA, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeSOCKS5, Host: "a1", Port: 1080, Enabled: true,
	})
	require.NoError(t, err, "供应商 A 可以用平台池已占用的名字（唯一域是每归属）")
	_, err = repos.Proxies.CreateProxy(ownerB, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeSOCKS5, Host: "b1", Port: 1080, Enabled: true,
	})
	require.NoError(t, err, "供应商 B 也能用同一个名字：既非跨租户命名冲突，也不构成存在性探测面")

	// ---- 同一供应商内重名：冲突 ----
	_, err = repos.Proxies.CreateProxy(ownerA, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeHTTP, Host: "a2", Port: 8080, Enabled: true,
	})
	require.ErrorIs(t, err, repository.ErrConflict, "同一供应商内重复 name → ErrConflict")
	require.Contains(t, err.Error(), `name="dup"`, "冲突详情含 name")

	// ---- 改名撞**同一归属**内的已有 name：冲突 ----
	secondID, err := repos.Proxies.CreateProxy(ownerA, domain.Proxy{
		Name: "dup-2", Scheme: domain.ProxySchemeHTTP, Host: "a3", Port: 8080, Enabled: true,
	})
	require.NoError(t, err)
	second, err := repos.Proxies.GetProxy(ownerA, secondID)
	require.NoError(t, err)
	second.Name = "dup"
	require.ErrorIs(t, repos.Proxies.UpdateProxy(ownerA, second, repository.ProxyCredential{}), repository.ErrConflict)

	// ---- 软删后同一归属内同名仍不可用（唯一索引与 deleted_at 无关）----
	require.NoError(t, repos.Proxies.DeleteProxy(ctx, id))
	_, err = repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeHTTP, Host: "h4", Port: 8080, Enabled: true,
	})
	require.ErrorIs(t, err, repository.ErrConflict,
		"软删不释放 name：唯一索引不含 deleted_at（与 template 同语义）")
	// 平台池软删掉的那条不影响别的归属用新名字（作用域隔离）。
	_, err = repos.Proxies.CreateProxy(ownerA, domain.Proxy{
		Name: "dup-fresh", Scheme: domain.ProxySchemeHTTP, Host: "a4", Port: 8080, Enabled: true,
	})
	require.NoError(t, err)
}

// TestPGDeleteProxyUnbindsAccounts 钉住删除连坐（spec §6.1）：软删代理的**同一
// 事务**内把指向它的账号 proxy_id 置 NULL（回落直连）；绑别的代理的账号与本就直连
// 的账号不受影响。
func TestPGDeleteProxyUnbindsAccounts(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-unbind")

	proxyA := seedPGProxyNamed(t, repos, "unbind-a")
	proxyB := seedPGProxyNamed(t, repos, "unbind-b")

	newAccount := func(name string) int64 {
		acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
			Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-" + name,
			MaxConcurrency: 8, Enabled: true,
		})
		require.NoError(t, err)
		return acc.ID
	}
	boundA := newAccount("bound-a")
	boundB := newAccount("bound-b")
	direct := newAccount("direct")

	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{boundA}, repository.AccountPatch{ProxyID: &proxyA})
	require.NoError(t, err)
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{boundB}, repository.AccountPatch{ProxyID: &proxyB})
	require.NoError(t, err)

	before, err := repos.Accounts.GetAccount(ctx, boundA)
	require.NoError(t, err)
	require.NotNil(t, before.ProxyID)
	require.Equal(t, proxyA, *before.ProxyID)

	require.NoError(t, repos.Proxies.DeleteProxy(ctx, proxyA))

	afterA, err := repos.Accounts.GetAccount(ctx, boundA)
	require.NoError(t, err)
	require.Nil(t, afterA.ProxyID, "删除代理 ⇒ 同事务解绑（proxy_id 落 NULL = 直连）")

	afterB, err := repos.Accounts.GetAccount(ctx, boundB)
	require.NoError(t, err)
	require.NotNil(t, afterB.ProxyID)
	require.Equal(t, proxyB, *afterB.ProxyID, "绑别的代理的账号不受影响")

	afterDirect, err := repos.Accounts.GetAccount(ctx, direct)
	require.NoError(t, err)
	require.Nil(t, afterDirect.ProxyID, "本就直连的账号保持 NULL")

	// 代理 B 仍在、代理 A 已软删。
	_, err = repos.Proxies.GetProxy(ctx, proxyA)
	require.ErrorIs(t, err, repository.ErrNotFound)
	_, err = repos.Proxies.GetProxy(ctx, proxyB)
	require.NoError(t, err)
}

// TestPGWriteProxyProbe 钉住探测写回：last_probe_at / last_probe_ok /
// last_probe_error 三个字段整批落值（注入固定时间，不依赖 time.Now 的毫秒差）。
func TestPGWriteProxyProbe(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	id := seedPGProxyNamed(t, repos, "probe-1")

	okAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Proxies.WriteProxyProbe(ctx, id, okAt, true, ""))

	got, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.LastProbeAt)
	require.True(t, got.LastProbeAt.Equal(okAt), "探测时刻落库并读回（want %v, got %v）", okAt, got.LastProbeAt)
	require.True(t, got.LastProbeOK)
	require.Empty(t, got.LastProbeError)

	// 失败探测覆盖三元组；探测结果不改条目可用性（§6.3：enabled 是唯一开关）。
	failAt := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, repos.Proxies.WriteProxyProbe(ctx, id, failAt, false, "dial tcp 10.0.0.9:1080: connection refused"))
	got, err = repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.LastProbeAt)
	require.True(t, got.LastProbeAt.Equal(failAt))
	require.False(t, got.LastProbeOK)
	require.Contains(t, got.LastProbeError, "connection refused")
	require.True(t, got.Enabled, "探测失败不改变 enabled（不影响 Routable）")

	// 已软删条目：探测写回视同不存在。
	require.NoError(t, repos.Proxies.DeleteProxy(ctx, id))
	require.ErrorIs(t, repos.Proxies.WriteProxyProbe(ctx, id, failAt, true, ""), repository.ErrNotFound)
}

// TestPGProxyUpdateKeepsProbeOwnership 钉住**列的写者归属**：探测三列只由
// WriteProxyProbe 写，管理面全量更新（UpdateProxy）不得顺带抹掉探测结论。
// 反例：改个 remark 就把 last_probe_* 清成「从未探测」——运维唯一的现场证据丢了，
// 而 PUT 的入参里根本没有这三列，调用方无从察觉。
func TestPGProxyUpdateKeepsProbeOwnership(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	id := seedPGProxyNamed(t, repos, "probe-owner")

	probedAt := time.Date(2026, 8, 3, 9, 30, 0, 0, time.UTC)
	require.NoError(t, repos.Proxies.WriteProxyProbe(ctx, id, probedAt, true, ""))

	// 全量更新：只改 remark（入参的探测三列是零值，若 UpdateProxy 写它们就会覆盖）。
	cur, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	cur.Remark = "edited"
	cur.LastProbeAt = nil
	cur.LastProbeOK = false
	cur.LastProbeError = ""
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, cur, repository.ProxyCredential{}))

	after, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "edited", after.Remark)
	require.NotNil(t, after.LastProbeAt, "全量更新不得清掉探测时刻（WriteProxyProbe 独占写）")
	require.True(t, after.LastProbeAt.Equal(probedAt), "探测时刻原样保留")
	require.True(t, after.LastProbeOK, "探测结论原样保留")
	require.Empty(t, after.LastProbeError)
}

// TestPGProxyUpdatePasswordTriState 钉住密码三态（spec §6.1 PUT 语义）：nil = 不变、
// &"" = 清空、&"v" = 落值；三态**不牵动**非密字段的「全量覆盖」语义，也不写探测
// 三列（探测列仍由 WriteProxyProbe 独占）。
//
// 为什么仓库层必须原子表达「不变」：若 service 用「先读旧密码再整体写回」，并发
// 的另一次改密码会被这次写回用读到的旧值静默覆盖（read-modify-write 窗口）。
func TestPGProxyUpdatePasswordTriState(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	id, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "tri-1", Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.1.1", Port: 1080,
		Username: "user1", Password: "pass1", Enabled: true,
	})
	require.NoError(t, err)

	probedAt := time.Date(2026, 8, 4, 7, 8, 9, 0, time.UTC)
	require.NoError(t, repos.Proxies.WriteProxyProbe(ctx, id, probedAt, true, ""))

	// 1) password = nil：旧密码原样保留，非密字段照改。
	cur, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	cur.Host = "10.2.2.2"
	cur.Port = 1081
	cur.Remark = "edited"
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, cur, repository.ProxyCredential{}))

	got, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "10.2.2.2", got.Host, "非密字段照改")
	require.Equal(t, 1081, got.Port)
	require.Equal(t, "edited", got.Remark)
	require.Equal(t, "pass1", got.Password, "password=nil ⇒ 不变（旧密码原样保留）")
	require.Equal(t, "user1", got.Username)
	require.NotNil(t, got.LastProbeAt, "全量更新不写探测三列")
	require.True(t, got.LastProbeOK)

	// 2) password = &""：清空（落 NULL → 读回空串）。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, got, repository.ProxyCredential{Password: new("")}))
	got, err = repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Empty(t, got.Password, "password 指向空串 ⇒ 清空")
	require.Equal(t, "user1", got.Username, "清密码不动 username")

	// 3) password = &"pass2"：落值。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, got, repository.ProxyCredential{Password: new("pass2")}))
	got, err = repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "pass2", got.Password)
	require.NotNil(t, got.LastProbeAt, "三态任一分支都不得写探测列")
	require.True(t, got.LastProbeOK)

	// 4) username 也是三态（与 password 同一对凭据，规矩必须一样）：
	//    nil ⇒ 不动它；只改 password 不影响 username。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, got, repository.ProxyCredential{Password: new("pass3")}))
	got, err = repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "user1", got.Username, "credential.Username = nil ⇒ username 不进 SET")
	require.Equal(t, "pass3", got.Password)

	// 5) username = &""：清空 username，password 同批落值（两列可在一次 UPDATE 里各自三态）。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, got, repository.ProxyCredential{
		Username: new(""), Password: new("pass4"),
	}))
	got, err = repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Empty(t, got.Username, "username 指向空串 ⇒ 清空")
	require.Equal(t, "pass4", got.Password)
	require.NotNil(t, got.LastProbeAt, "清 username 同样不得写探测列")
}

// --- 归属与 scope（spec-proxy-pool §3.6，M1d） ---

// seedPGProxyOwned 在**供应商作用域**下建条目：归属由 scope 钉死（请求体给不出 owner，
// 与真实写面同路径——不是直接写列）。
func seedPGProxyOwned(t *testing.T, repos *repository.Repository, uid int64, name string) int64 {
	t.Helper()
	id, err := repos.Proxies.CreateProxy(scopeCtx(uid), domain.Proxy{
		Name: name, Scheme: domain.ProxySchemeHTTP, Host: "proxy.example.com", Port: 3128, Enabled: true,
	})
	require.NoError(t, err)
	require.NotZero(t, id)
	return id
}

// TestPGProxyScopeIsolation 归属作用域矩阵（§3.6）：供应商面只看得到/改得动/删得掉
// **自己的**条目；平台代理（owner IS NULL）对供应商既不可见也不可写；管理面（无 scope）
// 看全部。越域与不存在**同码**（ErrNotFound，不泄露「别人有这条代理」）。
//
// 另钉两件事：CreateProxy 的归属来自 scope（请求体里填别人的 uid 无效），
// WriteProxyProbe 同样受作用域限制（供应商只能探测自己的出口）。
func TestPGProxyScopeIsolation(t *testing.T) {
	repos := newPGReposShared(t)
	adminCtx := context.Background()
	a := seedSupplierUser(t, repos, "scope-a@example.com")
	b := seedSupplierUser(t, repos, "scope-b@example.com")
	ctxA, ctxB := scopeCtx(a.ID), scopeCtx(b.ID)

	platformProxy := seedPGProxyNamed(t, repos, "scope-platform")
	proxyA := seedPGProxyOwned(t, repos, a.ID, "scope-a-proxy")
	proxyB := seedPGProxyOwned(t, repos, b.ID, "scope-b-proxy")

	// ① 请求体不可写归属：在 A 的作用域里把 OwnerUID 填成 B，落库仍是 A。
	smuggled, err := repos.Proxies.CreateProxy(ctxA, domain.Proxy{
		Name: "scope-smuggle", Scheme: domain.ProxySchemeSOCKS5, Host: "smuggle.example.com", Port: 1080,
		Enabled: true, OwnerUID: &b.ID,
	})
	require.NoError(t, err)
	got, err := repos.Proxies.GetProxy(adminCtx, smuggled)
	require.NoError(t, err)
	require.NotNil(t, got.OwnerUID)
	require.Equal(t, a.ID, *got.OwnerUID, "归属恒由作用域决定，请求体改不动")

	// ② 列表：供应商只出自己的；管理面看全部（含平台代理）。
	listA, err := repos.Proxies.ListProxies(ctxA)
	require.NoError(t, err)
	require.Equal(t, []int64{proxyA, smuggled}, proxyIDsOf(listA), "A 只看得到自己的两条")
	listB, err := repos.Proxies.ListProxies(ctxB)
	require.NoError(t, err)
	require.Equal(t, []int64{proxyB}, proxyIDsOf(listB))
	all, err := repos.Proxies.ListProxies(adminCtx)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{platformProxy, proxyA, proxyB, smuggled}, proxyIDsOf(all))

	// ③ 单读：越域与平台代理 ⇒ 同码 404；管理面读得到，且 owner 回显正确。
	_, err = repos.Proxies.GetProxy(ctxA, proxyB)
	require.ErrorIs(t, err, repository.ErrNotFound, "别人的私池 ⇒ 越域 404")
	_, err = repos.Proxies.GetProxy(ctxA, platformProxy)
	require.ErrorIs(t, err, repository.ErrNotFound, "平台代理不是公共池：供应商看不见")
	adminPlatform, err := repos.Proxies.GetProxy(adminCtx, platformProxy)
	require.NoError(t, err)
	require.Nil(t, adminPlatform.OwnerUID, "平台代理 owner 为 NULL")

	// ④ 更新：越域 ⇒ 404（连"存在"都不承认）。
	foreign, err := repos.Proxies.GetProxy(adminCtx, proxyB)
	require.NoError(t, err)
	foreign.Name = "scope-b-renamed"
	require.ErrorIs(t, repos.Proxies.UpdateProxy(ctxA, foreign, repository.ProxyCredential{}), repository.ErrNotFound)
	kept, err := repos.Proxies.GetProxy(adminCtx, proxyB)
	require.NoError(t, err)
	require.Equal(t, "scope-b-proxy", kept.Name, "越域更新不得落值")

	// ⑤ 探测写回同样带作用域。
	probeAt := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	require.ErrorIs(t, repos.Proxies.WriteProxyProbe(ctxA, proxyB, probeAt, true, ""), repository.ErrNotFound)
	require.NoError(t, repos.Proxies.WriteProxyProbe(ctxA, proxyA, probeAt, true, ""))
	probed, err := repos.Proxies.GetProxy(adminCtx, proxyA)
	require.NoError(t, err)
	require.True(t, probed.LastProbeOK)

	// ⑥ 删除：越域 ⇒ 404 且条目仍在。
	require.ErrorIs(t, repos.Proxies.DeleteProxy(ctxA, proxyB), repository.ErrNotFound)
	_, err = repos.Proxies.GetProxy(adminCtx, proxyB)
	require.NoError(t, err, "越域删除不得生效")
	require.NoError(t, repos.Proxies.DeleteProxy(ctxA, proxyA), "自己的删得掉")
	_, err = repos.Proxies.GetProxy(adminCtx, proxyA)
	require.ErrorIs(t, err, repository.ErrNotFound)
}

// proxyIDsOf 列表投影（断言只看 id 序）。
func proxyIDsOf(rows []domain.Proxy) []int64 {
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

// TestPGDeletePrivateProxyUnbindScope 删除连坐的**范围随归属**（§3.6）：删私池代理只解绑
// 该供应商的账号，别的供应商/平台账号的绑定原样保留；删平台代理仍解绑所有引用。
//
// 夹具里故意造出「B 的账号绑着 A 的代理」这种谓词禁止的状态：service 层拦住的是**新**
// 绑定，而解绑连坐必须对**历史遗留状态**也说清范围——它按列解绑，不按谓词过滤。
func TestPGDeletePrivateProxyUnbindScope(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-unbind-scope")
	a := seedSupplierUser(t, repos, "unbind-scope-a@example.com")
	b := seedSupplierUser(t, repos, "unbind-scope-b@example.com")
	ctxA := scopeCtx(a.ID)

	newAccount := func(name string, owner int64) int64 {
		acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
			Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-" + name,
			MaxConcurrency: 8, Enabled: true, SupplierUserID: owner,
		})
		require.NoError(t, err)
		return acc.ID
	}
	bind := func(accID, proxyID int64) {
		_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{accID}, repository.AccountPatch{ProxyID: &proxyID})
		require.NoError(t, err)
	}
	proxyIDOf := func(accID int64) *int64 {
		acc, err := repos.Accounts.GetAccount(ctx, accID)
		require.NoError(t, err)
		return acc.ProxyID
	}

	ownedByA := newAccount("unbind-owned-a", a.ID)
	ownedByB := newAccount("unbind-owned-b", b.ID)
	platformAcc := newAccount("unbind-platform", 0)

	privateProxyA := seedPGProxyOwned(t, repos, a.ID, "unbind-private-a")
	bind(ownedByA, privateProxyA)
	bind(ownedByB, privateProxyA)
	bind(platformAcc, privateProxyA)

	require.NoError(t, repos.Proxies.DeleteProxy(ctxA, privateProxyA), "供应商删自己的私池")
	require.Nil(t, proxyIDOf(ownedByA), "其主的账号解绑")
	require.NotNil(t, proxyIDOf(ownedByB), "别的供应商的绑定原样保留")
	require.Equal(t, privateProxyA, *proxyIDOf(ownedByB))
	require.NotNil(t, proxyIDOf(platformAcc), "平台账号的绑定同样原样保留")
	require.Equal(t, privateProxyA, *proxyIDOf(platformAcc))

	// 平台代理：既有行为——解绑所有引用（跨归属）。
	platformProxy := seedPGProxyNamed(t, repos, "unbind-platform-proxy")
	bind(ownedByA, platformProxy)
	bind(ownedByB, platformProxy)
	bind(platformAcc, platformProxy)
	require.NoError(t, repos.Proxies.DeleteProxy(ctx, platformProxy))
	require.Nil(t, proxyIDOf(ownedByA))
	require.Nil(t, proxyIDOf(ownedByB))
	require.Nil(t, proxyIDOf(platformAcc))
}

// TestPGLookupProxyForBinding 绑定谓词的事实读取（§3.6）：**跨 scope** 能读到别人的归属
// ——它判的是「能不能绑」而不是「能不能看」；软删 ⇒ ErrNotFound（与 GetProxy 同哨兵）。
func TestPGLookupProxyForBinding(t *testing.T) {
	repos := newPGReposShared(t)
	adminCtx := context.Background()
	a := seedSupplierUser(t, repos, "lookup-a@example.com")
	b := seedSupplierUser(t, repos, "lookup-b@example.com")

	privateProxyA := seedPGProxyOwned(t, repos, a.ID, "lookup-private-a")
	platformProxy := seedPGProxyNamed(t, repos, "lookup-platform")

	// ① 跨 scope：B 的作用域里读 A 的私池，读到的是 A 的归属。返回体只给归属这一个
	//    事实（「能不能绑」的全部输入），不含可用性/出口规格。
	info, err := repos.Proxies.LookupProxyForBinding(scopeCtx(b.ID), privateProxyA)
	require.NoError(t, err, "判据读取不受调用方作用域限制")
	require.NotNil(t, info.OwnerUID)
	require.Equal(t, a.ID, *info.OwnerUID)

	// ② 平台代理：OwnerUID 为 nil（谓词第一条分支的判据）。
	platformInfo, err := repos.Proxies.LookupProxyForBinding(scopeCtx(b.ID), platformProxy)
	require.NoError(t, err)
	require.Nil(t, platformInfo.OwnerUID, "平台代理 owner NULL ⇒ 只有管理员可分配")

	// ③ 停用条目仍可读：绑定校验只要求"存在且未软删"，停用不使它"假装不存在"
	//    （可用性由 Routable 在装载期派生，不是绑定判据的输入）。
	disabled := domain.Proxy{
		Name: "lookup-disabled", Scheme: domain.ProxySchemeHTTP, Host: "disabled.example.com", Port: 8080,
		Enabled: false,
	}
	disabledID, err := repos.Proxies.CreateProxy(adminCtx, disabled)
	require.NoError(t, err)
	disabledInfo, err := repos.Proxies.LookupProxyForBinding(adminCtx, disabledID)
	require.NoError(t, err, "停用条目的绑定校验仍读到行（存活即可判归属）")
	require.Nil(t, disabledInfo.OwnerUID, "管理面创建 ⇒ 平台代理")

	// ④ 软删 ⇒ 不存在。
	require.NoError(t, repos.Proxies.DeleteProxy(adminCtx, disabledID))
	_, err = repos.Proxies.LookupProxyForBinding(adminCtx, disabledID)
	require.ErrorIs(t, err, repository.ErrNotFound, "已软删视同不存在")
}
