// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/logx"
)

// CodexUsageSnapshotter codex 额度快照数据源（*sdkbridge.Codex 满足——装配侧
// 注入；接口化供测试注入，与 priceFetcher/local 同形态——依赖方向 service →
// sdkbridge 不反转）。
type CodexUsageSnapshotter interface {
	GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error)
}

// SetUsageSnapshotter 装配 codex 额度快照数据源（main：codexAdapter 构造后调
// 用；nil = 未装配——AccountUsage 对 codex 账号返回 nil 快照，不 panic）。
func (s *Service) SetUsageSnapshotter(u CodexUsageSnapshotter) { s.usageSnapshots = u }

// AccountUsage 账号 codex 额度快照（纯编排零基础设施——用户裁决 2026-08-18：
// 缓存/并发节流/失败冷却全在 sdkbridge，service 只做凭据取 + 类型判定 + 调
// 用；错误分类透传——ErrAuthExpired/ErrUpstream（sdkbridge 哨兵）供 task 2
// upstream_error 标记映射）。
//
// 数据流：store.GetAccountExt 取 ext 行（api-key 无 ext 行 → ErrNotFound →
// nil 快照零 sdkbridge 调用）→ CredentialFromExt 派生 cred（codex-oauth/
// codex-pat → 非 nil cred）→ sdkbridge.GetUsageSnapshot(ctx, cred)。
func (s *Service) AccountUsage(ctx context.Context, accountID int64) (*domain.CodexUsageSnapshot, error) {
	e, err := s.store.GetAccountExt(ctx, accountID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil // api-key（无 ext 行）→ nil 快照零 sdkbridge 调用
		}
		return nil, mapRepoErr(err)
	}
	cred := domain.CredentialFromExt(e)
	if cred.OAuthToken == "" && cred.OAuthRefreshToken == "" && cred.PATKey == "" {
		return nil, nil // 非 codex 凭据（防御——ext 行仅 codex 类型可写）→ nil 快照
	}
	if s.usageSnapshots == nil {
		return nil, nil // 未装配（测试/单实例形态）→ nil 快照
	}
	return s.usageSnapshots.GetUsageSnapshot(ctx, &cred)
}

// AccountsUsage 账号 usage 批量视图（/api/admin/accounts/usage 查询面——统一
// usage API spec 2026-08-18）：repo 单查询聚合 + 按 ids 顺序组装全量 items
// （无记录账号补零——gateway 全 0，前端免补零）+ upstream 装配（task 3
// AccountUsage：api-key 无凭据 → nil 快照/nil 标记；codex 成功 → 快照/nil；
// codex 失败 → nil/枚举标记——ErrAuthExpired → auth_expired，其余 →
// upstream_unavailable）。
//
// 失败语义：repo 聚合失败 → 整批失败（gateway 数据面不可用）；upstream 逐
// 账号装配失败 → 仅记 upstream_error 标记不整批失败（单账号快照挂不影响
// 其余账号 gateway 栏返回）。批内装配 errgroup 有界并发（8——与 sdkbridge
// usageFetchSem 容量对齐：上游并发仍由 sdkbridge 恒保 ≤8，此处仅并行化编排
// 的 DB 往返/调用分发——调度属编排面，缓存/节流仍全在 sdkbridge）；结果按
// account_ids 顺序组装（goroutine 按 index 写 out，保序）。非 sdkbridge 错误
// （store 故障——GetAccountExt 面）→ 记日志 + 该账号 null/null（不误标上游
// 问题，T2-2）。ids 去重/≤100 已由 handler 校验（service 兜底不再重复——
// 防御性校验由调用方边界承担，对齐既有批量端点惯例）。
func (s *Service) AccountsUsage(ctx context.Context, ids []int64, from, to time.Time) ([]domain.AccountUsage, error) {
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
	var g errgroup.Group
	g.SetLimit(8) // 批内并行度（与 sdkbridge usageFetchSem 语义对齐）
	for i := range out {
		i := i
		g.Go(func() error {
			snap, err := s.AccountUsage(ctx, out[i].AccountID)
			switch {
			case err == nil:
				out[i].Upstream = snap
			case errors.Is(err, sdkbridge.ErrAuthExpired):
				e := domain.UpstreamErrorAuthExpired
				out[i].UpstreamError = &e
			case errors.Is(err, sdkbridge.ErrUpstream):
				e := domain.UpstreamErrorUpstreamUnavailable
				out[i].UpstreamError = &e
			default:
				// 非 sdkbridge 错误（store 故障——GetAccountExt 面）→ 不误标
				// 上游问题：该账号 null/null（批内其余账号正常）。ctx 取消为
				// 请求已死信号，不记 Warn（非故障）。
				if ctx.Err() == nil && s.log != nil {
					s.log.Warn("accounts usage: account upstream lookup failed", logx.Int64("account_id", out[i].AccountID), logx.Error(err))
				}
			}
			return nil
		})
	}
	return out, g.Wait()
}
