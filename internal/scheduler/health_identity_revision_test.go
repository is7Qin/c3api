// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHealthEffectiveStateIsolatedByIdentityRevision 钉住健康记录的身份代际隔离：
// 健康记录按身份代际 K（identity_revision）隔离，同一账号在旧 K 下记录的
// OPEN 不得泄漏到新 K 的查询里。
//
// 为什么这是承重断言：健康事实描述的是「**某个身份**是否健康」。K 推进意味着
// 身份写入发生（管理员改动），旧身份的健康结论对新身份**不适用**——若泄漏，
// 一个刚被管理员修复/替换的账号会继续被当作不健康而跳过，或反之被当作健康
// 而上游 401。这也是 EffectiveState 必须显式收 K（而非泛化 revision）的原因：
// 泛化名会让调用点把客户端 CAS 令牌 C 静默传进来，两套语义就混在一起了。
func TestHealthEffectiveStateIsolatedByIdentityRevision(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	ctx := context.Background()

	// 旧 K=1 下记 OPEN（通配，覆盖该账号所有 quality）。
	_, err := h.Throttle(ctx, healthKeyFor(5, "*", 1), StateOPEN, 10*time.Minute)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))

	require.Equal(t, StateOPEN, h.EffectiveState(5, "q", 1),
		"同一 K 必须能观察到该记录（否则隔离断言是空洞的）")

	require.Equal(t, StateReady, h.EffectiveState(5, "q", 2),
		"旧 K 下的记录不得泄漏到新 K：健康事实按身份代际隔离")
}

// TestHealthClearAccountMustClearRedisNotJustMemory 反向证明 Redis 侧清理是强制的：
// 只清内存视图**不够**——Sync 会按活动 ZSET 把记录原样装回来，故 Redis 侧的
// 4 段清理（DEL 记录 + ZREM 活动 ZSET + HSET 墓碑哈希 + SET 墓碑前缀）是强制的。
//
// 本测试故意先走「天真做法」（只换内存视图）并**断言它失败**（记录复活），
// 再走完整原语并断言记录真正消失。若原语退化为只清内存，第一段断言就会
// 变成 StateReady 而失败——即本测试能抓住该退化。
func TestHealthClearAccountMustClearRedisNotJustMemory(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	ctx := context.Background()

	_, err := h.Throttle(ctx, healthKeyFor(7, "*", 1), StateOPEN, 10*time.Minute)
	require.NoError(t, err)
	// 同账号另一 quality + 另一账号：用于证明清理按账号边界生效。
	_, err = h.Throttle(ctx, healthKeyFor(7, "q2", 1), StateOPEN, 10*time.Minute)
	require.NoError(t, err)
	_, err = h.Throttle(ctx, healthKeyFor(8, "*", 1), StateOPEN, 10*time.Minute)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, StateOPEN, h.EffectiveState(7, "q", 1))
	require.Equal(t, StateOPEN, h.EffectiveState(8, "q", 1))

	// —— 天真做法：只清内存视图。Redis 侧记录仍在活动 ZSET 中 ——
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{}})
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, StateOPEN, h.EffectiveState(7, "q", 1),
		"只清内存视图不足以清理：Sync 会按活动 ZSET 把未过期的 OPEN 记录装回视图")

	// —— 完整原语：4 段纪律（含墓碑）——
	n, err := h.ClearAccount(ctx, 7)
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "必须恰好清掉账号 7 的两个字段（q 通配 + q2）")
	require.NoError(t, h.Sync(ctx))

	require.Equal(t, StateReady, h.EffectiveState(7, "q", 1),
		"完整清理后不得复活（墓碑阻止 Sync 保留）")
	require.Equal(t, StateReady, h.EffectiveState(7, "q2", 1),
		"该账号全部 quality 一并清掉")
	require.Equal(t, StateOPEN, h.EffectiveState(8, "q", 1),
		"清理必须按账号边界生效：不得清到别的账号（前缀匹配 accId .. ':'）")
}

// TestHealthClearAccountIsolatesByAccountBoundary 单独钉住账号边界：清理使用
// `accountID .. ':'` 前缀匹配，故账号 7 的清理**不得**影响账号 70/17（它们与
// 7 共享数字前缀，但字段格式 accountID:quality:revision 使前缀匹配结构上安全）。
func TestHealthClearAccountIsolatesByAccountBoundary(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	ctx := context.Background()

	for _, acc := range []int64{7, 70, 17} {
		_, err := h.Throttle(ctx, healthKeyFor(acc, "*", 1), StateOPEN, 10*time.Minute)
		require.NoError(t, err)
	}
	require.NoError(t, h.Sync(ctx))

	n, err := h.ClearAccount(ctx, 7)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "只应清掉账号 7 自己")
	require.NoError(t, h.Sync(ctx))

	require.Equal(t, StateReady, h.EffectiveState(7, "q", 1))
	require.Equal(t, StateOPEN, h.EffectiveState(70, "q", 1), "数字前缀相邻的账号不得被误清")
	require.Equal(t, StateOPEN, h.EffectiveState(17, "q", 1), "数字后缀相邻的账号不得被误清")
}
