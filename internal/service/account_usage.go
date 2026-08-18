// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
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
