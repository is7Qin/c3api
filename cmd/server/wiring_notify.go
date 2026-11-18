// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 装配阶段 3/6：余额预警投递面（Redis 冷却去重 + 邮件 worker）。
//
// 本阶段刻意只产两件东西：`notification.Cooldown`（余额预警已知键清理，管理面
// Deps 与 wireBalanceWarning 共用同一实例）与 `service.MailWorker`（邮件队列）。
// 依赖它的 billing 阶段才构造 warningW —— 那里同时需要 svc（预警判定）与
// mailW（投递），把 warningW 放这里会与 billing 成环。
package main

import (
	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/notification"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/settingssnap"
	"github.com/is7qin/c3api/pkg/logx"
)

// notifyFace 是阶段 3 的产物。
type notifyFace struct {
	cooldown *notification.Cooldown
	mailW    *service.MailWorker
}

// notifyDeps 是阶段 3 **实际从 infra 阶段读到的全部字段**（窄化，spec §7.2）。
//
// 为什么不直接收 *infraFace：那样本阶段可以触达 infra 造的任何东西（连接池、
// 出口池、codex 客户端、实例发现…），而其中只有 3 个是它真需要的。窄结构把
// 「本阶段需要什么」变成一行可 review 的投影，未来新增 infra 产物也不会默认
// 对每个阶段可见。
type notifyDeps struct {
	rdb          *redis.Client
	repos        *repository.Repository
	settingsSnap *settingssnap.Snapshot
}

// newNotifyFace 装配阶段 3（只依赖 notifyDeps）。
func newNotifyFace(log *logx.Logger, d notifyDeps) *notifyFace {
	rdb, repos := d.rdb, d.repos
	settingsSnap := d.settingsSnap
	f := &notifyFace{}

	// 余额预警已知键清理（Redis）：构造期一次建好，Deps 与
	// wireBalanceWarning 共用同一实例。
	bwCooldown := notification.NewCooldown(rdb)
	f.cooldown = bwCooldown
	// mailW 与 svc 同源共享同一个 *settingssnap.Snapshot（NOTIFY 只刷一处，无
	// 分叉）；mailW.Enqueue 经 Deps.MailEnqueue 一次注入，零 Set* 回填。
	mailW := service.NewMailWorker(service.MailDeps{Log: log, Settings: settingsSnap, Templates: repos})
	f.mailW = mailW

	return f
}
