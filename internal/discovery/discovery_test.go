// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package discovery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/redisx"
)

// 测试基座（consumer spec §4 场景全覆盖；miniredis 确定性——等待一律走
// require.Eventually 轮询谓词 + Stats().ConsecutiveErrors 失败信号，零 sleep）：
//
//	剪除语义按"提交的 cutoff"判定（score 与本进程时钟比较），miniredis 的
//	FastForward 只推进服务端时钟（管 EXPIRE/TTL），无法老化客户端提交的 score
//	——故死亡剪除用"注入陈旧 score 成员"驱动，整键 EXPIRE 用 FastForward 驱动，
//	两条腿各自确定性覆盖。
//
// 客户端经 redisx.Open 构造：dogfood 全仓唯一构造点纪律（foundation spec §2.2）。

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	return mr, c
}

func startDiscovery(t *testing.T, c *redis.Client, self string) *Discovery {
	t.Helper()
	d := New(c, self, nil)
	require.NoError(t, d.Start(t.Context()))
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

func zcard(t *testing.T, c *redis.Client) int64 {
	t.Helper()
	n, err := c.ZCard(t.Context(), MembersKey).Result()
	require.NoError(t, err)
	return n
}

func zmembers(t *testing.T, c *redis.Client) []string {
	t.Helper()
	ms, err := c.ZRange(t.Context(), MembersKey, 0, -1).Result()
	require.NoError(t, err)
	return ms
}

// TestClusterInstancesNeverBelowOne 首个 tick 前（未 Start）读数 = 1：下限 clamp
// 契约（永不返回 0，consumer spec §2.3）。
func TestClusterInstancesNeverBelowOne(t *testing.T) {
	_, c := newTestRedis(t)
	d := New(c, "cold", nil)
	require.Equal(t, 1, d.ClusterInstances(), "无有效观测 → 单实例语义 1")
}

// TestStatsWireFormat 锁定 Stats 的线上键形（ops/workers 契约同步义务）：json
// 序列化后为小写 snake_case 三键对象——漂移即 openapi.yaml WorkerStatus.stats
// 清单过期（handler 侧 TestGetOpsWorkersDiscoveryContract 同款镜像）。
func TestStatsWireFormat(t *testing.T) {
	b, err := json.Marshal(Stats{Instances: 2, LastTickOk: false, ConsecutiveErrors: 4})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	require.Equal(t, float64(2), m["instances"])
	require.Equal(t, false, m["last_tick_ok"])
	require.Equal(t, float64(4), m["consecutive_errors"])
	require.Len(t, m, 3, "无多余/缺失键")
}

// TestHeartbeatRegistersSingleInstance 场景 §4.1+§4.5 合一：心跳注册 ZCARD=1
// （Fix 2 同步首 tick：Start 返回即注册，无需等异步循环）；跨多次心跳稳态 N≡1
// （以 score 前移证明第二次心跳确已发生，非单点快照）。
func TestHeartbeatRegistersSingleInstance(t *testing.T) {
	_, c := newTestRedis(t)
	d := startDiscovery(t, c, "inst-a")

	require.Equal(t, 1, d.ClusterInstances(), "同步首 tick：Start 返回即注册且 N=1")
	require.Equal(t, []string{"inst-a"}, zmembers(t, c))

	s1, err := c.ZScore(t.Context(), MembersKey, "inst-a").Result()
	require.NoError(t, err)
	var s2 float64
	require.Eventually(t, func() bool {
		s2, err = c.ZScore(t.Context(), MembersKey, "inst-a").Result() // 第二次心跳 score 必前移
		return err == nil && s2 > s1
	}, 5*time.Second, 20*time.Millisecond, "后续 tick 持续续跳")
	require.Equal(t, 1, d.ClusterInstances(), "单实例稳态 N≡1")
	require.Equal(t, int64(1), zcard(t, c))
}

// TestTwoInstancesConvergeWithinOneTick 场景 §4.1 双实例 + 验收 §5 推演（Fix 2 后
// 语义收口）：后启动者的同步首 tick 即见先启动者已注册的成员——Start 返回瞬间
// Stats().Instances=2；先启动者经 ≤1 个异步 tick 感知新成员。双方 ≤1 tick 收敛。
func TestTwoInstancesConvergeWithinOneTick(t *testing.T) {
	_, c := newTestRedis(t)
	a := startDiscovery(t, c, "inst-a")
	b := startDiscovery(t, c, "inst-b")

	require.Equal(t, 2, b.Stats().(Stats).Instances,
		"同步首 tick：b 的 Start 返回即持有真实 N=2（无 N=1 预算窗口）")
	require.Equal(t, 2, b.ClusterInstances())
	require.Eventually(t, func() bool { return a.ClusterInstances() == 2 },
		heartbeatInterval+2*time.Second, 10*time.Millisecond,
		"先启动者 a 经 ≤1 个异步 tick 收敛到 2")
	require.Equal(t, int64(2), zcard(t, c))
}

// TestDeadMemberPrunedNextTick 场景 §4.2 死亡剪除：第三成员停止续跳（score 停在
// >memberTTL 前）→ 存活实例下一 tick 剪除，计数回落。先以新鲜 score 证其被计数，
// 再回拨 score 模拟死亡——两段均由 Eventually 谓词锚定，无竞态窗口。
func TestDeadMemberPrunedNextTick(t *testing.T) {
	_, c := newTestRedis(t)
	d := startDiscovery(t, c, "inst-a")

	now := float64(time.Now().UnixMilli())
	require.NoError(t, c.ZAdd(t.Context(), MembersKey, redis.Z{Score: now, Member: "inst-dead"}).Err())
	require.Eventually(t, func() bool { return d.ClusterInstances() == 2 }, 3*time.Second, 10*time.Millisecond,
		"新鲜 score 的第三成员被计入")

	stale := float64(time.Now().UnixMilli()) - float64((memberTTL+time.Second)/time.Millisecond)
	require.NoError(t, c.ZAdd(t.Context(), MembersKey, redis.Z{Score: stale, Member: "inst-dead"}).Err())
	require.Eventually(t, func() bool {
		return d.ClusterInstances() == 1 && zmembers(t, c)[0] == "inst-a"
	}, 3*time.Second, 10*time.Millisecond, "下一 tick 剪除死者，计数回落 1")
}

// TestWholeKeyExpiresViaFastForward EXPIRE 腿（§2.1 整键防遗弃累积）：键 TTL 由
// 心跳 pipeline 的 EXPIRE 命令设置——经一次真实心跳产生带 TTL 的键，实例离开后
// 遗留成员随整键自灭。FastForward 推进 miniredis 服务端时钟加速 TTL 到期。
func TestWholeKeyExpiresViaFastForward(t *testing.T) {
	mr, c := newTestRedis(t)
	d := startDiscovery(t, c, "inst-a")
	require.Eventually(t, func() bool { return zcard(t, c) == 1 }, 3*time.Second, 10*time.Millisecond,
		"真实心跳已建键（含 EXPIRE 20s）")

	now := float64(time.Now().UnixMilli())
	require.NoError(t, c.ZAdd(t.Context(), MembersKey, redis.Z{Score: now, Member: "abandoned"}).Err())
	require.NoError(t, d.Close(context.Background())) // 实例离开：ZREM 自身，键与遗留成员仍在

	mr.FastForward(keyTTL + time.Second)
	require.Zero(t, zcard(t, c), "全体实例离场后整键 EXPIRE 自灭，遗弃成员不累积")
}

// TestScaleDownImmediateAfterGracefulStop 场景 §4.3 缩容即时性：Close 同步 ZREM
// 自身 → 存活实例下一 tick N 减一（不等 TTL）。同时验证 Close 幂等。
func TestScaleDownImmediateAfterGracefulStop(t *testing.T) {
	_, c := newTestRedis(t)
	a := startDiscovery(t, c, "inst-a")
	b := startDiscovery(t, c, "inst-b")
	require.Eventually(t, func() bool { return a.ClusterInstances() == 2 }, 3*time.Second, 10*time.Millisecond)

	require.NoError(t, b.Close(context.Background()))
	require.NotContains(t, zmembers(t, c), "inst-b", "Close 已同步 ZREM 自身")
	require.Eventually(t, func() bool { return a.ClusterInstances() == 1 }, 3*time.Second, 10*time.Millisecond,
		"存活实例下一 tick 感知缩容")
	require.NoError(t, b.Close(context.Background()), "Close 幂等")
}

// TestCloseBeforeStartSafe worker 契约：未 Start 直接 Close 安全 no-op 不 panic。
func TestCloseBeforeStartSafe(t *testing.T) {
	_, c := newTestRedis(t)
	d := New(c, "never-started", nil)
	require.NoError(t, d.Close(context.Background()))
}

// TestFreezeOnRedisOutageThenRecover 场景 §4.4 故障冻结 + 恢复：miniredis 关闭 →
// tick 连续失败（Stats().ConsecutiveErrors 为确定性信号）但 ClusterInstances 冻结
// 上次值（不 0 不 panic）；同端口重启 Redis → 自动续跳恢复计数。
func TestFreezeOnRedisOutageThenRecover(t *testing.T) {
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.StartAddr("127.0.0.1:0"))
	t.Cleanup(func() { mr.Close() })
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })

	a := startDiscovery(t, c, "inst-a")
	b := startDiscovery(t, c, "inst-b")
	require.Eventually(t, func() bool {
		return a.ClusterInstances() == 2 && b.ClusterInstances() == 2
	}, 3*time.Second, 10*time.Millisecond, "停机前双方已收敛 N=2（冻结值才有意义）")

	addr := mr.Addr() // Addr 在 Close 后不可读（内部锁），先取
	mr.Close()        // Redis 停机演练（单测内模拟）
	as := a.Stats().(Stats)
	require.Eventually(t, func() bool {
		s := a.Stats().(Stats)
		return s.ConsecutiveErrors >= as.ConsecutiveErrors+1
	}, 8*time.Second, 20*time.Millisecond, "tick 失败可观测（冻结语义已进入）")
	require.Equal(t, 2, a.ClusterInstances(), "冻结上次 N=2")
	require.Equal(t, 2, b.ClusterInstances(), "另一实例同款冻结")
	require.False(t, a.Stats().(Stats).LastTickOk)

	restarted := miniredis.NewMiniRedis()
	require.NoError(t, restarted.StartAddr(addr), "同端口重启（连接池自动重连的前提）")
	t.Cleanup(func() { restarted.Close() })
	require.Eventually(t, func() bool {
		sa, sb := a.Stats().(Stats), b.Stats().(Stats)
		return sa.LastTickOk && sb.LastTickOk &&
			sa.Instances == 2 && sb.Instances == 2 &&
			sa.ConsecutiveErrors == 0 && sb.ConsecutiveErrors == 0
	}, 8*time.Second, 20*time.Millisecond, "恢复后自动续跳，双方 ≤ 数 tick 重新收敛 N=2")
}
