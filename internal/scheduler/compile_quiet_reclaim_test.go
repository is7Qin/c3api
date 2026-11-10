// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 配置静默期的资源回收义务（§3.3b-1/§3.3b-4）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// TestQuietBackstopReclaimsLateResource 配置长期静默时，已移除账号的迟到资源仍会被
// 回收：请求侧可能在上一批 `Accounts()` 快照之后、批次应用之前为已移除账号建出条目，
// 而点删除只能由"账号全集差分"产生——全集差分只在有批次下发时发生。删掉无条件周期
// reload 之后，静默轮是唯一的兜底机会，所以它必须下发一批资源指令。
//
// 判据全是资源指针与查询次数：只跑静默轮（探针命中 + 完整视图 + 无 reload 义务），
// 不触发 full reload（loader 计数不变）。
func TestQuietBackstopReclaimsLateResource(t *testing.T) {
	table := sdkbridge.NewClientTable()
	adapter := sdkbridge.NewCodex(nil, nil, table, sdkbridge.RotationDeps{})

	tplChat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplChat, 4)}})
	cl := &countingLoader{inner: m}
	cfg := testCfg()
	cfg.ClientResources = table
	s := newSchedWithCfg(t, cfg, cl)

	// 静默轮的三个前提：探针命中（tuple 未变）、无 reload 义务、已发布完整视图。
	c := m.probeCounts()
	s.lastProbe.Store(&c)
	s.stalenessProbe = func(context.Context) (compileProbeCounts, error) { return c, nil }
	require.True(t, s.publishedViewWhole(), "前置：整视图已发布（静默轮只在整视图上跳过重载）")
	genBefore := s.accounts.gen

	// 账号 1 仍在配置全集里；账号 99 已从配置移除，但请求侧在快照之后建出了条目。
	_, err := adapter.Prepare(&domain.AccountCredential{AccountID: 1, PATKey: "pat-1", IdentityRevision: 1})
	require.NoError(t, err)
	_, err = adapter.Prepare(&domain.AccountCredential{AccountID: 99, PATKey: "pat-99", IdentityRevision: 1})
	require.NoError(t, err)
	require.NotNil(t, table.Get(99), "前置：已移除账号的条目确实存在")
	require.Len(t, s.clientAccounts(), 2)

	loads := cl.loadsN()
	s.backstopTick(context.Background())

	require.Nil(t, table.Get(99), "已移除账号的迟到条目必须被点名删除")
	require.NotNil(t, table.Get(1), "仍在账号全集里的条目保留")
	require.Len(t, s.clientAccounts(), 1)
	require.Equal(t, loads, cl.loadsN(), "静默轮不得重扫配置（不触发 full reload）")
	require.Equal(t, genBefore+1, s.accounts.gen, "静默轮仍取一个新批次号（围栏随之前进）")
}
