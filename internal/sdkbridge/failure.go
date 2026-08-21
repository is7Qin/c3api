// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package sdkbridge 是 SDK 适配层与网关之间的契约面（T1，零 SDK 依赖——SDK
// 调用从 T2 起）：统一失效回调 + 失效处理链装配（写失效字段 / 调度摘除 /
// 审计）、网关侧信封错误（P2-1）与凭据传递形态（AccountCredential 派生在
// internal/domain）。
package sdkbridge

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// FailureHandler 账号失效上报的唯一入口：SDK 适配层（T2/T4 起）把 SDK 内部判死
// （OnAuthFatal / errors.As fatal 四类——RefreshOAuthError / AuthPermanentlyRevokedError /
// AccountDisabledError / CallbackDeliveryError；RefreshError 可重试，有意排除）翻译成一次统一回调；
// 网关侧只处理这一个入口。
//
// 语义：
//   - 账号级终止（凭据永久失效/上游封禁/判死）才上报；**可重试类（RefreshError
//     等）不上报**（网关按既有 failover 分类处理）
//   - **信封类错误不上报**（透传协议——网关 statusOf/upstreamErrMsg 零改动复用）
//   - 双源去重：rotationAuth 路径同一 fatal 既触发 OnAuthFatal 又随返回错误
//     errors.As 命中——**以回调为准去重、单次上报**（结构或语义级去重，T2
//     适配层实现；本契约只定义回调形态）
type FailureHandler func(accountID int64, fatal error)

// FailureStore 失效字段落库面（repository.AccountRepo 满足；接口化供测试注入
// 与装配侧解耦）。
type FailureStore interface {
	// SetAccountFailed 幂等写 failed_at + last_error（失效原因文本，复用既有
	// last_error——用户裁决 2026-08-13：两原因字段并存会漂移；失效后账号摘除
	// 不再被调度，普通失败写点不会覆盖失效原因，复用安全）：failed_at 已置
	//（首次上报）→ 不覆盖（保持首次失效时刻与原因；重复上报不重复写）。
	SetAccountFailed(ctx context.Context, accountID int64, failedAt time.Time, reason string) error
}

// AccountFailer 调度摘除面（*scheduler.Scheduler 满足；接口化供测试注入）。
type AccountFailer interface {
	// FailAccount 快照置 StatusDisabled + last_error 审计 + 经 loader 持久化
	//（重启快照重载后仍摘除——pickFrom 只跳 disabled 不查 failed_at，必须落库）。
	FailAccount(accountID int64, reason string)
}

// FailureDeps 失效处理链依赖（main 装配：repository.Accounts + scheduler）。
type FailureDeps struct {
	Store  FailureStore
	Failer AccountFailer
	// Log 处理错误日志（P3-1 评审：同一失败只记一条——记在回调侧
	// NewFailureHandler，HandleFailure 不重复记）；nil = no-op。
	Log *logx.Logger
}

// HandleFailure 网关侧失效处理链（T1 §3——统一回调装配；T2/T4 适配层在
// FailureHandler 回调中调用；冷面——失败上报低频）：
//
//  1. DB 写 failed_at + last_error（失效原因文本，复用既有 last_error——用户
//     裁决 2026-08-13：两原因字段并存会漂移；幂等：重复上报不重复写，首次
//     失效时刻保持）
//  2. 调度器状态置 StatusDisabled（快照摘除 + 经既有 loader 持久化 + last_error
//     审计随回写落库）——复用既有 pickFrom 过滤器（跳 disabled）与 MarkResult
//     防复活守卫（置位后在途请求结果短路）
//  3. 失败请求自身不在此链——由 proxy 既有分类路径处理（fatal → 连接级
//     MarkResult 分流，failover 不重试同一账号；forward.go 语义，T1 不改动）
//
// DB 写失败不阻断摘除（fail-closed：账号已判死，摘除优先；错误返回供日志）。
// 返回 DB 写错误（nil = 成功）；调度摘除为 void（快照外账号 no-op）。
// 本函数不记日志——处理错误统一由回调侧（NewFailureHandler）记一条（P3-1
// 评审：同一失败不得双条 Warn）。
func HandleFailure(ctx context.Context, deps FailureDeps, accountID int64, fatal error) error {
	if fatal == nil {
		return nil // 防御：无错误不上报
	}
	reason := domain.TruncateErrMsg(fatal.Error())
	err := deps.Store.SetAccountFailed(ctx, accountID, time.Now(), reason)
	// 摘除恒执行：DB 故障时内存摘除先生效，恢复后 writeback 落库（fail-closed）。
	deps.Failer.FailAccount(accountID, reason)
	return err
}

// NewFailureHandler 构造统一失效回调（网关侧唯一失效处理入口）：适配层构造时
// 注册，账号级终止经此上报；回调内同步执行失效处理链（写字段/调度摘除/审计）。
// 回调签名不返回错误（契约固定）——处理链错误在回调内记**一条**日志
// （deps.Log；nil 则不记——P3-1 评审：同一失败单条 Warn，含 account_id + 错误）。
func NewFailureHandler(deps FailureDeps) FailureHandler {
	return func(accountID int64, fatal error) {
		if err := HandleFailure(context.Background(), deps, accountID, fatal); err != nil && deps.Log != nil {
			deps.Log.Warn("account failure handling failed", logx.Int64("account_id", accountID), logx.Error(err))
		}
	}
}
