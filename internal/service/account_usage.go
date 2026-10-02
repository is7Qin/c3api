// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// 本文件只留网关聚合 + 凭据组装 + 失效冻结判定三个纯 helper：codex 额度快照的
// fan-out 编排（含上游调用与错误分类）已整体搬到 handler 层（AdminAPI 经
// 构造注入的 CodexUsageProber 直调适配器）——service 不再持有快照数据源，
// 不再 import sdkbridge。

// AccountUsageCredential 账号 codex 凭据组装（纯数据面零上游调用）：
// store.GetAccountExt 取 ext 行（api-key 无 ext 行 → ErrNotFound → nil/nil）
// → CredentialFromExt 派生 cred（codex-oauth/codex-pat 列组）→ 非 codex
// 凭据（全空）→ nil/nil。调用方（handler fan-out）凭 nil/non-nil 分流：
// nil = 无上游能力（null 快照），non-nil = 调 prober。
func (s *Service) AccountUsageCredential(ctx context.Context, accountID int64) (*domain.AccountCredential, error) {
	e, err := s.store.GetAccountExt(ctx, accountID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil // api-key（无 ext 行）→ 无上游能力
		}
		mapped := mapRepoErr(err)
		// 非 ErrNotFound store 故障 → Warn + 透错（handler 侧记 null/null，
		// 不误标上游问题；ctx 取消为请求已死信号，不记 Warn）。
		if ctx.Err() == nil && s.log != nil {
			s.log.Warn("accounts usage: account upstream lookup failed", logx.Int64("account_id", accountID), logx.Error(mapped))
		}
		return nil, mapped
	}
	cred := domain.CredentialFromExt(e)
	if cred.OAuthToken == "" && cred.OAuthRefreshToken == "" && cred.PATKey == "" {
		return nil, nil // 非 codex 凭据（防御——ext 行仅 codex 类型可写）→ 无上游能力
	}
	return &cred, nil
}

// AccountUsageFrozen 失效冻结判定：账号是否处于运行时失效（failed_at 置位）——
// SDK/rule 判死并已从调度摘除的账号。调用方（handler usage 装配）据此**冻结**
// 上游额度拉取：失效账号的额度查询无意义，且对其拨号会反复触发 SDK 凭据刷新
// 判死上报（token revoked 循环 → C 无限增长）。与 Enabled=false（管理面手动
// 禁用）语义分离——手动禁用的账号仍可查额度，只有 failed_at 冻结（Q1 裁决）。
//
// 返回 (frozen, failureSource, err)：frozen=true 时 failureSource 为
// account.failure_source（domain.FailureSourceSDK/Rule，空 = 未记来源的旧路径），
// 供调用方按来源决定 upstream_error 标记（Q2：sdk → 凭据失效；rule → 不标）。
// 账号缺 id（ErrNotFound）→ (false, "", nil)（无对象可冻结）；其余 store 故障透传。
func (s *Service) AccountUsageFrozen(ctx context.Context, accountID int64) (bool, string, error) {
	a, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return false, "", nil
		}
		return false, "", err
	}
	if a.FailedAt == nil {
		return false, "", nil
	}
	if a.FailureSource != nil {
		return true, *a.FailureSource, nil
	}
	return true, "", nil
}

// AccountsGatewayUsage 账号网关用量批量聚合（/api/admin/accounts/usage 查询面
// 的 gateway 栏）：先过 domain.Admit（KindUsageAgg——raw 成本上限 90d + usage_logs
// 覆盖率），再 repo 单查询聚合 + 按 ids 顺序组装全量 items（无记录账号补零
// ——gateway 全 0，前端免补零）。repo 聚合失败 → 整批失败（gateway 数据面不
// 可用）。upstream 栏由调用方（handler fan-out）另行装配。
func (s *Service) AccountsGatewayUsage(ctx context.Context, ids []int64, from, to time.Time) ([]domain.AccountUsage, error) {
	// KindUsageAgg 无分组、只有原始行一种候选存储（Storages[0]），故执行方法
	// 单一；窗口取 Exec（判定与执行同源）。该处旧实现只校验 from < to（handler
	// 层），全区间 GROUP BY 无 LIMIT ⇒ 本判定补掉该成本洞（spec §4.5）。
	if _, err := s.admitStats(domain.KindUsageAgg, time.UTC, from, to); err != nil {
		return nil, err
	}
	aggs, err := s.store.ScanUsageAgg(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]domain.AccountUsage, 0, len(ids))
	for _, id := range ids {
		item := domain.AccountUsage{AccountID: id}
		if a := aggs[id]; a != nil {
			item.Gateway = *a
		}
		out = append(out, item)
	}
	return out, nil
}
