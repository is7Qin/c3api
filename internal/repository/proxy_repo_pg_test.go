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
	got.Username = ""
	got.Password = ""
	got.Remark = ""
	got.Enabled = false
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, got))

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

	// 更新幂等：同值再写一遍不报错、结果不变。
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, updated))
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
	require.ErrorIs(t, repos.Proxies.UpdateProxy(ctx, updated), repository.ErrNotFound)
}

// TestPGProxyNameUnique 钉住 name 唯一的**既有语义**（与 template/group 同）：
// 唯一索引不含 deleted_at ⇒ 软删后同名仍冲突（删除只影响可见性，不释放名字）。
func TestPGProxyNameUnique(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	id, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeSOCKS5, Host: "h1", Port: 1080, Enabled: true,
	})
	require.NoError(t, err)

	_, err = repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeHTTP, Host: "h2", Port: 8080, Enabled: true,
	})
	require.ErrorIs(t, err, repository.ErrConflict, "重复 name → ErrConflict（service 映射 409）")
	require.Contains(t, err.Error(), `name="dup"`, "冲突详情含 name")

	// 改名撞已有 name 同样冲突。
	secondID, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup-2", Scheme: domain.ProxySchemeHTTP, Host: "h3", Port: 8080, Enabled: true,
	})
	require.NoError(t, err)
	second, err := repos.Proxies.GetProxy(ctx, secondID)
	require.NoError(t, err)
	second.Name = "dup"
	require.ErrorIs(t, repos.Proxies.UpdateProxy(ctx, second), repository.ErrConflict)

	// 软删后同名仍不可用（唯一索引与 deleted_at 无关——与 template 语义一致）。
	require.NoError(t, repos.Proxies.DeleteProxy(ctx, id))
	_, err = repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "dup", Scheme: domain.ProxySchemeHTTP, Host: "h4", Port: 8080, Enabled: true,
	})
	require.ErrorIs(t, err, repository.ErrConflict,
		"软删不释放 name：唯一索引不含 deleted_at（与 template 同语义）")
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
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, cur))

	after, err := repos.Proxies.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "edited", after.Remark)
	require.NotNil(t, after.LastProbeAt, "全量更新不得清掉探测时刻（WriteProxyProbe 独占写）")
	require.True(t, after.LastProbeAt.Equal(probedAt), "探测时刻原样保留")
	require.True(t, after.LastProbeOK, "探测结论原样保留")
	require.Empty(t, after.LastProbeError)
}
