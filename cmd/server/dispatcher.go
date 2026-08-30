// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"sync/atomic"

	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/pkg/logx"
)

// schedGroupPub 把 notify.Publisher 适配为 scheduler.GroupChangePublisher
// （scheduler 不 import notify——发布面接口化（与 service.Publisher 同模式），
// 装配侧粘合）。账号状态回写成功后发组级 NOTIFY（Change.Groups），接收端
// Dispatcher.Apply → Accounts(gids, false) → 组级定向重载。
type schedGroupPub struct{ p *notify.Publisher }

func (a schedGroupPub) PublishGroups(ctx context.Context, gids []int64) {
	_ = a.p.Publish(ctx, notify.Change{Groups: gids}) // 失败 Publisher 内部已 Warn，60s 兜底收敛
}

type settingsReloader interface {
	ReloadSettings(ctx context.Context) error
}

// dispatcher 实现 notify.Dispatcher（#14 T3a 装配侧）：把 NOTIFY Change 转发
// 给 invalidate 去抖器的 Mark 方法（本地/远端变更共享同一去抖窗口，天然合并
// 去重——设计文档 §2.3）；settings 变更例外——同步 ReloadSettings 后再经快照
// 注册表按 scope 精确重载（#36：N 变更/auth 预算即时生效，时序见 Apply）；
// FullRefresh 经注册表全量刷新（监听器连接成功兜底，R8；首连跳过见
// FullRefresh 注释——E2 启动双刷）。
//
// 放装配侧（cmd/server）而非 notify 包：notify 不 import invalidate/service
// 是 T1 设计约束（避免依赖环），适配只能在依赖两者的最外层做。
type dispatcher struct {
	inv       *invalidate.Debouncer
	svc       settingsReloader   // *service.Service（Apply settings 分支同步刷新 + FullRefresh）
	snapshots *snapshot.Registry // 五路快照注册表（NOTIFY scope 分发 + 断线重连全量刷新）
	log       *logx.Logger       // nil = 静默（测试）
	// bootLoaded 启动首刷全成功标志（E2 启动双刷）：main 在注册表 ReloadAll
	// 返回空 map（全部成功）后置位、wm.StartAll 之前（程序序保证监听器首连必
	// 见标志）；FullRefresh 首个调用（= 监听器首连）CAS 消费——命中则跳过五路
	// ReloadAll（单实例健康启动下第二遍纯冗余：大表启动 DB 负载/就绪延迟约
	// 翻倍），仅补 ReloadSettings（svc 不在注册表内，保持既有语义）。未置位
	// （首刷部分失败 → 首连仍全量，兜底收敛不破坏）或已消费（断线重连 → 恒
	// 全量刷新）均走全量路径。多实例 pre-LISTEN 漏窗 ≤30s sched 同步 / 60s
	// auth-sync 兜底收敛，可接受。
	bootLoaded atomic.Bool
}

// Apply 处理一条 NOTIFY 变更：按映射表转去抖器 Mark（设计文档 §2.2/§2.3）。
// 映射表：
//   - Users → Users()：auth + 余额快照全量
//   - Templates → Templates()：sched 全量 + clients 失效
//   - Groups（±Clients）→ Accounts(gids, keyChanged)：sched 组级定向；账号
//     upstream_key 变更带 Clients → 同批 clients 失效
//   - Clients（独立）→ Clients()：仅客户端工厂失效（服务端恒与 Templates/
//     Groups 并排，防御性兜底）
//   - Multipliers → Multipliers()：余额倍率快照定向刷新
//   - Keys → Keys()：auth 快照全量（key CRUD 缺口）
//   - Settings → 同步 ReloadSettings（快照先刷新——#36 时序，去抖 Mark 由
//     同步重载取代）+ 注册表按 ScopeSettings 精确重载声明方（当前 = auth：
//     gate 预算按新 N 重算，#36）
//   - Rules → Rules()：规则表全量重载（重载清窗口计数，全实例同步语义）
//
// 合并语义：Templates + Groups 同窗（载荷守卫降级 full）→ 去抖器 merge 后
// 组级被全量包含跳过，语义仍正确。除 settings 分支（同步 ReloadSettings 一
// 次 DB 读——低频路径，时序见上）外 Mark 路径零锁零 DB。无返回值：内部失败
// 独立 Warn 消化，不透传（G-P2-1：NOTIFY 是事件提示，调用方无任何可执行动
// 作，透传只会造成 listener 侧双 Warn；模块周期 ticker / 60s 兜底已存在）。
func (d *dispatcher) Apply(ctx context.Context, ch notify.Change) {
	if ch.Users {
		d.inv.Users()
	}
	if ch.Templates {
		d.inv.Templates()
	}
	switch {
	case ch.Clients && len(ch.Groups) > 0:
		d.inv.Accounts(ch.Groups, true) // 账号 upstream_key 变更：组级重载 + clients 失效（一次 mark 合并）
	case ch.Clients:
		d.inv.Clients()
	case len(ch.Groups) > 0:
		d.inv.Accounts(ch.Groups, false)
	}
	if ch.Multipliers {
		d.inv.Multipliers()
	}
	if ch.Keys {
		d.inv.Keys()
	}
	if ch.Settings {
		// #36 即时重算时序（R2 M-1）：先同步刷新 settings 快照（ReloadSettings，
		// N 立即入快照），再按 scope 精确重载声明方（auth Reload → gate.reload →
		// allocBudget 现读 N 即时重分配预算）——顺序保证预算读到新 N。修复前
		// 旧实现仅 Mark（200ms 去抖后才 flush ReloadSettings），
		// reloadScopes 同步 auth.Reload 读到旧 N = 白重算；新 N 落地后再无
		// gate.reload 触发。settings 为低频路径，同步 DB 读可接受；去抖 Mark 由
		// 本次同步重载取代（免 200ms 后重复 ReloadSettings），失败 Warn + 模块
		// 周期 ticker / 下次变更兜底收敛。
		if err := d.svc.ReloadSettings(ctx); err != nil && d.log != nil {
			d.log.Warn("settings reload failed", logx.Error(err))
		}
		d.reloadScopes(ctx, snapshot.ScopeSettings)
	}
	if ch.Rules {
		d.inv.Rules()
	}
}

// reloadScopes 注册表按 scope 精确重载（nil 注册表 = 未装配，no-op）。错误
// 独立 Warn——NOTIFY 是事件提示，失败由各模块周期 ticker / 60s 兜底收敛。
func (d *dispatcher) reloadScopes(ctx context.Context, scopes ...string) {
	if d.snapshots == nil {
		return
	}
	for name, err := range d.snapshots.Reload(ctx, scopes...) {
		logSnapshotReloadErr(d.log, "snapshot scope reload failed", name, err)
	}
}

// FullRefresh 监听器每次连接成功（启动首连 / 断线重连）时的本地刷新（设计
// 文档 §2.3 / R8）：注册表 ReloadAll（auth + scheduler + rules + pricing +
// balances）覆盖断连期间 NOTIFY 丢失，另重载 settings 快照（svc——不在注册
// 表内，保持既有语义）。各步独立尽力执行，返回首个错误（listener 侧 Warn）。
// 调用方无需区分首连/重连——是否跳过由本方法裁决（E2 启动双刷）：main 启动
// 首刷全成功置位 bootLoaded 后，首个调用（= 首连）CAS 消费即跳过五路
// ReloadAll、仅补 ReloadSettings——单实例健康启动下第二遍纯冗余（大表启动
// DB 负载/就绪延迟约翻倍）；多实例 pre-LISTEN 漏窗 ≤30s sched 同步 / 60s
// auth-sync 兜底收敛，可接受。标志未置（首刷部分失败）或已消费（断线重连）
// → 恒全量刷新不变。
func (d *dispatcher) FullRefresh(ctx context.Context) error {
	if d.bootLoaded.CompareAndSwap(true, false) {
		// 首连 + 启动首刷全成功：五路 ReloadAll 已在 main 启动序全部成功执行，
		// 重复为纯冗余——跳过，仅补 ReloadSettings（svc 不在注册表内）。
		return d.svc.ReloadSettings(ctx)
	}
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if d.snapshots != nil {
		// 逐项先记录（helper 带 panic stack 诊断），再保持现有首错返回语义——
		// 多 snapshot 失败诊断不因首错选择丢失。
		for name, err := range d.snapshots.ReloadAll(ctx) {
			logSnapshotReloadErr(d.log, "snapshot reload failed", name, err)
			record(err)
		}
	}
	record(d.svc.ReloadSettings(ctx))
	return firstErr
}
