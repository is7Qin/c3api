// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package discovery Redis 实例发现（spec 2026-08-25-redis-instance-discovery-design，
// 基建面见 2026-08-25-redis-foundation-design）：单 ZSET 心跳成员协议替换手工
// cluster.instances 设置——实例心跳注册、活体计数即多实例预算分摊基数 N。
//
// 协议（consumer spec §2.1）：key c3api:cluster:members；member = 实例 ID
// （hostname-pid-bootnonce，复用 notify/publisher.go Src 生成模式，main 注入同一值）；
// score = 最近心跳 unixmilli。单个心跳循环每 tick 一条 pipeline 同时完成心跳与计数：
//
//	ZREMRANGEBYSCORE <key> -inf <now-15s>  -- 剪除死成员（按提交的 cutoff 判定）
//	ZADD             <key> <now> <self>     -- 自刷新（先于 ZCARD ⇒ 计数恒含自身）
//	EXPIRE           <key> 20s              -- 整键防遗弃累积
//	ZCARD            <key>                  -- 活体数 N → atomic.Int32
//
// 故障语义（consumer spec §2.3）：pipeline 报错 → 冻结上次 N + 节流告警（≥30s 一次）
// ——与旧世界 stale-N 同级退化，绝不 fail-closed、绝不返回 0（下限 clamp 1）；
// go-redis 连接池自带重连，Redis 恢复后自动续心跳（恢复打一次 Info）。
//
// PR-notes（consumer spec §2.2「实现者必查」——gate 对 provider 的消费时机调查结论）：
// concurrencyGate.instancesN() 在每次预算分配（reload/upsert/复核 allocBudget）实时
// 调 provider.ClusterInstances()（internal/proxy/gate.go:106-121 注释明示"N 在每次
// 预算分配现读，下次分配即生效"）；limit RPM 同款现读（internal/proxy/limit.go:60）。
// 本包 atomic 写即天然生效，传播时延 ≤1 tick（≈1s），无需 reload 回调/刷新钩子。
//
// 可观测（foundation spec §2.4）：Stats() 实现 handler.StatsProvider，经 main
// 装配进 GET /api/admin/ops/workers 聚合；json 键清单与 openapi.yaml
// WorkerStatus.stats description 同步维护（同步义务钉在 handler/ops.go 接口注释）。
package discovery

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/pkg/logx"
)

// 协议常量（consumer spec §2.1：命名常量不做配置项）。
const (
	// MembersKey 成员 ZSET 键。协调态可丢，键名带 c3api: 命名空间防共用实例串库。
	MembersKey = "c3api:cluster:members"
	// heartbeatInterval 心跳周期：N 变化感知 ≤1 tick。
	heartbeatInterval = time.Second
	// memberTTL 成员判死阈值：score 早于 now-memberTTL 视为死亡剪除。
	// 偏斜预算（clock-skew margin）：score 取自各实例应用侧时钟而非 Redis TIME，
	// 观察者 O 剪成员 M 的判据是 M 的观测年龄 > memberTTL，而观测年龄含心跳相位
	// [0,heartbeatInterval] + 成对时钟偏斜 s。3s 档的偏斜余量仅 ~1-2s——跨物理机
	// 未管 VM 漂移即可触发静默误剪（N 少算 → 多实例预算超卖，且 tick 全成功、
	// 监控无感）；15s 给到 ≈13s 偏斜容忍，NTP 级绰绰有余（隐含依赖：宿主机
	// NTP 正常）。代价是死成员自动检测延迟 3s→15s、缩容自动收敛变慢——方向
	// 保守可接受：主动缩容走 Close 的即时 ZREM，不经过此阈值。
	memberTTL = 15 * time.Second
	// keyTTL 整键过期（20s > memberTTL + heartbeatInterval）：存活集群内剪除
	// 恒先于过期生效，过期只在全体实例同时死亡后自灭键——防遗弃成员永久累积。
	keyTTL = 20 * time.Second
	// warnThrottle 故障告警节流间隔（≥30s 一次，consumer spec §2.3）。
	warnThrottle = 30 * time.Second
	// tickTimeout 单次 pipeline 预算（≤1 tick，超时按失败计走冻结语义）。
	tickTimeout = time.Second
	// zremTimeout 停机 ZREM 独立预算（best-effort，不挤占排空主预算）。
	zremTimeout = 2 * time.Second
)

// Stats 可观测快照（foundation spec §2.4 alive N / lastTickOk / consecutiveErrors；
// "enabled" 字段随 Redis 必选化失去意义——无"未启用"分支，不再保留常 true 噪声字段）。
// json 小写键对齐 ops/workers 契约惯例（openapi.yaml WorkerStatus.stats 清单，
// 与其它 worker 同款 snake_case）；Go 字段名不变。
type Stats struct {
	Instances         int   `json:"instances"`         // 当前生效 N（含故障冻结值；≥1）
	LastTickOk        bool  `json:"last_tick_ok"`      // 最近 tick 是否成功（false = 冻结中）
	ConsecutiveErrors int64 `json:"consecutive_errors"` // 连续失败次数（恢复归零）
}

// Discovery ZSET 心跳成员发现：实现 worker.Worker（Name/Start/Close） +
// proxy.InstancesProvider 形态（ClusterInstances() int）。客户端经 pkg/redisx
// 单点构造注入（foundation spec §2.2 全仓唯一构造点纪律），本包不自建连接。
type Discovery struct {
	client *redis.Client
	self   string // 实例 ID（main 的 instanceSrc 产物，与 NOTIFY Src 同源）
	log    *logx.Logger

	n       atomic.Int32 // 活体计数（0 = 尚无有效观测，读侧 clamp 1）
	errs    atomic.Int64 // 连续 tick 失败数（恢复归零；Stats 与测试的确定性信号）
	lastWarn atomic.Int64 // 上次 Warn unixnano（节流窗口；仅心跳 goroutine 读写）
	failed   atomic.Bool  // 上次 tick 失败标志（恢复 Info 只打一次）

	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// New 构造发现器（零副作用：Start 才起 goroutine，Close 未 Start 时安全）。
func New(client *redis.Client, self string, log *logx.Logger) *Discovery {
	return &Discovery{client: client, self: self, log: log}
}

// Name worker 名（worker.Worker 契约）。
func (d *Discovery) Name() string { return "discovery" }

// Start 非阻塞启动心跳循环（幂等；自持 ctx——排空由 Close 驱动，与装配传参解耦，
// 同 billing/usage 的 baseCtx/baseCancel 惯用法）。启动窗口收口：startOnce 内
// goroutine 起跑前同步执行首个心跳——Start 返回即持有真实 N（wm.StartAll 先于
// 流量入口 ⇒ N=1 预算窗口归零；redisx.Open 已 Ping 通过，正常路径毫秒级，
// 预算受 tickTimeout 约束）。首 tick 失败仅经 onTickError Warn 一句，照常进入
// 异步循环——Redis 必选但运行期故障冻结语义不变（绝不 fail-closed 启动）。
func (d *Discovery) Start(_ context.Context) error {
	d.startOnce.Do(func() {
		d.tick(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		d.cancel, d.done = cancel, make(chan struct{})
		go func() {
			defer close(d.done)
			t := time.NewTicker(heartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					d.tick(ctx)
				}
			}
		}()
	})
	return nil
}

// Close 幂等优雅停机（consumer spec §2.3）：关循环后 ZREM 自身（best-effort）——
// 缩容立即掉出其余实例的 N，不等 member TTL。注册序保证反序排空时本 Close 先于
// 游标终扫执行（foundation spec §2.3 装配序）。
func (d *Discovery) Close(ctx context.Context) error {
	d.stopOnce.Do(func() {
		if d.cancel == nil {
			return // 未 Start 过：Close 安全 no-op（worker 契约）
		}
		d.cancel()
		select {
		case <-d.done:
		case <-ctx.Done(): // 排空预算耗尽：Warn 继续退出，不阻塞停机链
			if d.log != nil {
				d.log.Warn("discovery: loop stop timed out", logx.Error(ctx.Err()))
			}
		}
		// WithoutCancel：停机链 ctx 已取消/将取消，ZREM 用独立短预算尽力而为。
		zctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), zremTimeout)
		defer cancel()
		if err := d.client.ZRem(zctx, MembersKey, d.self).Err(); err != nil && d.log != nil {
			d.log.Warn("discovery: zrem self failed", logx.String("self", d.self), logx.Error(err))
		}
	})
	return nil
}

// ClusterInstances 当前活体实例数 N（proxy.InstancesProvider 形态）。首个 tick 前
// 与故障冻结期恒 ≥1（consumer spec §2.3 永不返回 0；首个 tick 前返回 1 =
// 单实例语义，Redis 必选后无"未配置"分支）。
func (d *Discovery) ClusterInstances() int {
	if n := d.n.Load(); n > 0 {
		return int(n)
	}
	return 1
}

// Stats 可观测快照（handler.StatsProvider 形态；errs 是测试断言故障冻结的确定性
// 信号——替代 time.Sleep 等待失败发生）。
func (d *Discovery) Stats() any {
	errs := d.errs.Load()
	return Stats{
		Instances:         d.ClusterInstances(),
		LastTickOk:        errs == 0,
		ConsecutiveErrors: errs,
	}
}

// tick 单条 pipeline 心跳+计数（命令按发送序执行：剪除→自刷→续期→计数，
// ZCARD 恒含自身且不含死者）。失败走冻结语义，成功清错误态并补恢复日志。
func (d *Discovery) tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()
	now := time.Now().UnixMilli()
	pipe := d.client.Pipeline()
	pipe.ZRemRangeByScore(ctx, MembersKey, "-inf", strconv.FormatInt(now-int64(memberTTL/time.Millisecond), 10))
	pipe.ZAdd(ctx, MembersKey, redis.Z{Score: float64(now), Member: d.self})
	pipe.Expire(ctx, MembersKey, keyTTL)
	card := pipe.ZCard(ctx, MembersKey)
	if _, err := pipe.Exec(ctx); err != nil {
		d.onTickError(err)
		return
	}
	// 自身已在集合（ZADD 先行），ZCARD 理论 ≥1；max 防御 clamp 保持"永不 <1"
	// 不变量的单一出处（consumer spec §2.3）。
	n := max(card.Val(), 1)
	d.n.Store(int32(n))
	d.errs.Store(0)
	if d.failed.CompareAndSwap(true, false) && d.log != nil {
		d.log.Info("discovery: redis recovered, resuming heartbeat",
			logx.String("self", d.self), logx.Int("instances", int(n)))
	}
}

// onTickError 冻结上次 N + 节流告警（≥warnThrottle 一次；仅心跳 goroutine 调用，
// 无并发）。N 不动 = 冻结；恢复由下一个成功 tick 自然续上。
func (d *Discovery) onTickError(err error) {
	d.errs.Add(1)
	d.failed.Store(true)
	if d.log == nil {
		return
	}
	if time.Since(time.Unix(0, d.lastWarn.Load())) < warnThrottle {
		return
	}
	d.lastWarn.Store(time.Now().UnixNano())
	d.log.Warn("discovery: heartbeat failed, freezing last instance count",
		logx.String("self", d.self),
		logx.Int("instances", d.ClusterInstances()),
		logx.Int64("consecutive_errors", d.errs.Load()),
		logx.Error(err))
}
