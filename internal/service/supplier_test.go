// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// supplier_test.go 供应商业务面 service（spec 2026-10-09 §6.2/I5）：具名资金操作者
// 透传到 store（A24②：claims.Ver 真实到达资金 Actor）；I5 失败映射 403。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestApplySupplierSettlementPassesActor(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	// A24②：具名操作者（uid + 签名请求 ver）经 service 到达 store。
	actor := domain.FundsActor{UserID: 42, TokenVersion: 7}
	out, err := svc.ApplySupplierSettlement(context.Background(), domain.ApplySettlementRequest{
		OperatorUID: 42, SupplierUID: 42, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 100, RequestKey: "k1",
	}, actor)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Equal(t, actor, fs.lastSupplierApplyActor, "具名操作者（含 token_version）必须到达 store")

	// I5 失败（静态 token/陈旧 ver/角色不可达）⇒ 403（ErrForbidden）。
	fs.supplierApplyErr = repository.ErrFundsForbidden
	_, err = svc.ApplySupplierSettlement(context.Background(), domain.ApplySettlementRequest{
		OperatorUID: 42, SupplierUID: 42, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 100, RequestKey: "k2",
	}, actor)
	require.ErrorIs(t, err, ErrForbidden)

	// 金额 ≤ 0 / request_key 空 ⇒ 400（参数校验先于 store）。
	_, err = svc.ApplySupplierSettlement(context.Background(), domain.ApplySettlementRequest{
		OperatorUID: 42, SupplierUID: 42, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 0, RequestKey: "k3",
	}, actor)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.ApplySupplierSettlement(context.Background(), domain.ApplySettlementRequest{
		OperatorUID: 42, SupplierUID: 42, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 100, RequestKey: "",
	}, actor)
	require.ErrorIs(t, err, ErrInvalidInput)
}
