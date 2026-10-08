// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// supplier_admin.go 管理面结算审批 service（spec 2026-10-09 §6.3/§6.4/§6.5）：
// 结算单列表 + 五态 CAS（approve/reject/claim/confirm-failed/paid）+ 代申请 +
// 余额列表/PATCH。资金命令一律要求具名 JWT 操作者（domain.FundsActor），
// 写事务内的复核在 repository 完成（I5）。
//
// 错误映射：repository.ErrNotFound → ErrNotFound(404)；ErrConflict/ErrStaleRevision
// → ErrConflict(409)；ErrFundsForbidden → ErrForbidden(403)；ErrInvalidInput
// → ErrInvalidInput(400)。

import (
	"context"
	"errors"
	"strings"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/supplier"
)

// mapSupplierAdminErr 管理面结算错误映射（含 I5 的 403）。
func mapSupplierAdminErr(err error) error {
	if errors.Is(err, repository.ErrFundsForbidden) {
		return ErrForbidden
	}
	return mapRepoErr(err)
}

// AdminListSupplierSettlements 管理面结算单列表（§6.3）。
func (s *Service) AdminListSupplierSettlements(ctx context.Context, filter domain.SupplierSettlementFilter, limit, offset int) ([]*domain.SupplierSettlement, int64, error) {
	return s.store.ListSupplierSettlementsAdmin(ctx, filter, limit, offset)
}

// AdminListSupplierBalances 管理面余额列表（§6.3）。
func (s *Service) AdminListSupplierBalances(ctx context.Context, limit, offset int) ([]domain.SupplierBalance, int64, error) {
	return s.store.ListSupplierBalances(ctx, limit, offset)
}

// AdminPatchSupplierBalance PATCH 逐供应商配置（§6.3/§6.4 A16⑤）：**仅**
// share_bp / freeze_hours（不接受金额）；复用 config 同一组校验函数（含跨字段
// 上界，按全局 g）。
func (s *Service) AdminPatchSupplierBalance(ctx context.Context, uid int64, p domain.SupplierBalancePatch) (*domain.SupplierBalance, error) {
	if uid <= 0 {
		return nil, ErrInvalidInput
	}
	if p.ShareBp != nil {
		if err := config.ValidateSupplierShareBp(*p.ShareBp); err != nil {
			return nil, ErrInvalidInput
		}
	}
	if p.FreezeHours != nil {
		if *p.FreezeHours < 0 {
			return nil, ErrInvalidInput
		}
		// 跨字段上界只在全局粒度已知时判定（g <= 0 = 未装配：仅值域）。
		if s.supplierGranularity > 0 {
			if err := config.ValidateSupplierFreezeHours(*p.FreezeHours, s.supplierGranularity); err != nil {
				return nil, ErrInvalidInput
			}
		}
	}
	out, err := s.store.PatchSupplierBalance(ctx, uid, p)
	if err != nil {
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}

// AdminApproveSettlement pending → approved。
func (s *Service) AdminApproveSettlement(ctx context.Context, id, expectedRevision int64, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	out, err := s.store.ApproveSettlement(ctx, id, expectedRevision, actor)
	if err != nil {
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}

// AdminRejectSettlement pending|approved → rejected（退还 available）。
func (s *Service) AdminRejectSettlement(ctx context.Context, id, expectedRevision int64, reason *string, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	out, err := s.store.RejectSettlement(ctx, id, expectedRevision, reason, actor)
	if err != nil {
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}

// AdminClaimSettlement approved → paying（认领 + 风控门；§6.5）。金额必须 == 单据
// 金额；收款目标快照结构化（收款人/账号/单位均非空）；风控证据非空。
func (s *Service) AdminClaimSettlement(ctx context.Context, id, expectedRevision, amountMillis int64, payee domain.SupplierPayeeSnapshot, riskEvidence string, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if amountMillis <= 0 {
		return nil, ErrInvalidInput
	}
	if strings.TrimSpace(payee.PayeeName) == "" || strings.TrimSpace(payee.Account) == "" || strings.TrimSpace(payee.Unit) == "" {
		return nil, ErrInvalidInput
	}
	if strings.TrimSpace(riskEvidence) == "" {
		return nil, ErrInvalidInput
	}
	out, err := s.store.ClaimSettlement(ctx, id, expectedRevision, amountMillis, payee, riskEvidence, actor)
	if err != nil {
		if errors.Is(err, supplier.ErrPayoutGate) || errors.Is(err, supplier.ErrRiskReview) {
			return nil, ErrInvalidInput
		}
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}

// AdminConfirmFailedSettlement paying → approved（仅结构化「确定未支付」+「旧执行
// 已停止」核验；未知 ⇒ 失败闭合保留 paying）。
func (s *Service) AdminConfirmFailedSettlement(ctx context.Context, id, expectedRevision int64, in domain.SupplierPayoutFailureConfirmation, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if strings.TrimSpace(in.Reason) == "" || strings.TrimSpace(in.Evidence) == "" || !in.ConfirmedNotPaid || !in.OldExecutionStopped {
		return nil, ErrInvalidInput
	}
	out, err := s.store.ConfirmFailedSettlement(ctx, id, expectedRevision, in, actor)
	if err != nil {
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}

// AdminPaidSettlement paying → paid（lifetime_paid 累加；不重做风控门；external_ref
// 必须非空）。
func (s *Service) AdminPaidSettlement(ctx context.Context, id, expectedRevision int64, externalRef string, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if strings.TrimSpace(externalRef) == "" {
		return nil, ErrInvalidInput
	}
	out, err := s.store.PaidSettlement(ctx, id, expectedRevision, externalRef, actor)
	if err != nil {
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}

// AdminRequestSettlement 代申请（§6.3 admin_request）：目标须已有余额行（404）；
// 金额 > 0；request_key 必填；写事务内复核操作者（I5）。
func (s *Service) AdminRequestSettlement(ctx context.Context, req domain.ApplySettlementRequest, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if req.SupplierUID <= 0 || req.AmountMillis <= 0 || req.RequestKey == "" {
		return nil, ErrInvalidInput
	}
	out, err := s.store.AdminApplySettlement(ctx, req, actor)
	if err != nil {
		return nil, mapSupplierAdminErr(err)
	}
	return out, nil
}
