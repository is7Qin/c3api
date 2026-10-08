// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// supplier.go 供应商业务面 service（spec 2026-10-09 §6.1/§6.2）：全部以 JWT 本人
// uid 为作用域（handler 从 claims 取 uid 传入）。申请结算的幂等/条件扣语义在
// repository 单事务内保证（§6.2 I3），service 只做参数与错误映射。

import (
	"context"
	"errors"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// SupplierOverview 供应商收益概览（§6.1）。
func (s *Service) SupplierOverview(ctx context.Context, uid int64) (*domain.SupplierOverview, error) {
	return s.store.SupplierOverview(ctx, uid)
}

// SupplierChunks 冻结中明细（§6.1）。
func (s *Service) SupplierChunks(ctx context.Context, uid int64, limit, offset int) ([]domain.SupplierChunk, int64, error) {
	return s.store.SupplierChunks(ctx, uid, limit, offset)
}

// SupplierEarnings 区间收益明细（§6.1）。
func (s *Service) SupplierEarnings(ctx context.Context, uid int64, limit, offset int) ([]domain.SupplierEarning, int64, error) {
	return s.store.SupplierEarnings(ctx, uid, limit, offset)
}

// SupplierSettlements 我的结算单列表（§6.1）。
func (s *Service) SupplierSettlements(ctx context.Context, uid int64, limit, offset int) ([]*domain.SupplierSettlement, int64, error) {
	return s.store.ListSupplierSettlements(ctx, uid, limit, offset)
}

// ApplySupplierSettlement 申请结算（§6.2）：amount > 0 且 request_key 非空；
// 条件扣 + 期间链 + 幂等在 repository 单事务完成；**写事务内复核具名操作者
// （I5：状态/角色/token_version）**——actor 缺失/陈旧 ⇒ ErrForbidden(403)。
func (s *Service) ApplySupplierSettlement(ctx context.Context, req domain.ApplySettlementRequest, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if req.AmountMillis <= 0 {
		return nil, ErrInvalidInput
	}
	if req.RequestKey == "" {
		return nil, ErrInvalidInput
	}
	out, err := s.store.ApplySettlement(ctx, req, actor)
	if err != nil {
		if errors.Is(err, repository.ErrFundsForbidden) {
			return nil, ErrForbidden
		}
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrConflict
		}
		if errors.Is(err, repository.ErrInvalidInput) {
			return nil, ErrInvalidInput
		}
		return nil, mapRepoErr(err)
	}
	return out, nil
}
