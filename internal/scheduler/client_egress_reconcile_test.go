// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestReloadReconcilesClientEgress 配置面回收的**接线**：每次全量 reload / 组级批重载
// 都把「全量账号集 → 当前 responses 出口」交给 ClientTable（§3.2 出口/transport 变更）。
//
// 没有这条接线，ClientTable.Reconcile 就是死代码：活跃集永远停在使用请求兜底登记的那
// 一版，代理 CRUD 之后旧摘要不会被摘、usage 面的判断也没有配置可依。
func TestReloadReconcilesClientEgress(t *testing.T) {
	pool := egress.NewPool(nil)
	px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.2.3", Port: 1080,
		Username: "u", Password: "p1", Enabled: true}
	tplA := tpl(1, domain.FormatOpenAIResponses, []string{"gpt-4o"})
	bound := proxyAcc(t, 1, tplA, px)
	direct := acc(2, tplA, 4)
	loader := newMemLoader(map[int64][]*domain.Account{10: {bound, direct}})

	var got []map[int64]*egress.Client
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ReconcileClients = func(active map[int64]*egress.Client) { got = append(got, active) }
	s := newSchedWithCfg(t, cfg, loader)

	require.Len(t, got, 1, "全量 reload 必须触发一次配置面回收")
	require.Contains(t, got[0], int64(1), "全量账号集：绑定代理的账号")
	require.Contains(t, got[0], int64(2), "全量账号集：直连账号（活跃集恒含直连）")
	require.Same(t, pool.Direct(), got[0][2], "未绑定 = 直连出口（零值摘要）")
	require.Equal(t, egress.New("socks5", "10.1.2.3", 1080, "u", "p1").Key(), got[0][1].Spec.Key(),
		"绑定账号的活跃集 = 它当前配置的出口摘要")

	// 代理密码轮换（配置变更）：装载器像生产一样给出**新的**对象（新 Proxy 值 + 新账号），
	// 下一次 reload 传的就是新摘要——旧摘要不在集合里，该出口下的桶由 Reconcile 摘除。
	px2 := *px
	px2.Password = "p2"
	loader.set(10, []*domain.Account{proxyAcc(t, 1, tplA, &px2), direct})
	require.NoError(t, s.InvalidateAllSync())
	require.Len(t, got, 2, "重载必须再次交出活跃集")
	require.Equal(t, egress.New("socks5", "10.1.2.3", 1080, "u", "p2").Key(), got[1][1].Spec.Key(),
		"新配置的出口摘要（旧摘要不再出现 ⇒ 历史规格被摘除）")
	require.NotContains(t, [][16]byte{got[1][1].Spec.Key()}, egress.New("socks5", "10.1.2.3", 1080, "u", "p1").Key())

	// 账号消失 ⇒ 集合里不再有它（整账号摘除的判定输入，含没有会话池的账号）。
	loader.set(10, []*domain.Account{direct})
	require.NoError(t, s.InvalidateAllSync())
	require.Len(t, got, 3)
	require.NotContains(t, got[2], int64(1), "被删除的账号不再出现在活跃集里")
	require.Contains(t, got[2], int64(2))
}
