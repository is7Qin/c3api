// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"testing"

	"github.com/oapi-codegen/nullable"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAccountPatchFromBodySupplierUserID §2.5 唯一字段级例外：
//   - 管理面：正整数 ⇒ 分配目标（&uid）；显式 null ⇒ 清空（&0）；缺席 ⇒ nil。
//   - 供应商面：请求体出现该字段（含 null）⇒ 400（归属恒为 JWT 本人，不可改）。
func TestAccountPatchFromBodySupplierUserID(t *testing.T) {
	mgmt := context.Background()

	assign := AccountConfigPatch{SupplierUserId: nullable.NewNullableWithValue(int64(9))}
	p, err := accountPatchFromBody(mgmt, &assign)
	require.NoError(t, err)
	require.NotNil(t, p.SupplierUserID)
	require.Equal(t, int64(9), *p.SupplierUserID)

	clear := AccountConfigPatch{SupplierUserId: nullable.NewNullNullable[int64]()}
	p, err = accountPatchFromBody(mgmt, &clear)
	require.NoError(t, err)
	require.NotNil(t, p.SupplierUserID)
	require.Equal(t, int64(0), *p.SupplierUserID, "null = 清空（回平台自有）")

	absent := AccountConfigPatch{}
	p, err = accountPatchFromBody(mgmt, &absent)
	require.NoError(t, err)
	require.Nil(t, p.SupplierUserID, "缺席 = 不变")

	supplierCtx := domain.WithAccountScope(mgmt, domain.SupplierAccountScope(7))
	_, err = accountPatchFromBody(supplierCtx, &assign)
	require.Error(t, err, "供应商面出现 supplier_user_id ⇒ 400")
	_, err = accountPatchFromBody(supplierCtx, &clear)
	require.Error(t, err, "供应商面出现 supplier_user_id: null ⇒ 400")

	// 供应商面未提及该字段 ⇒ 正常（服务端钉死 jwtUser）。
	{
		name := "acc"
		tid := int64(1)
		body := AccountConfigPatch{Name: nullable.NewNullableWithValue(name), TemplateId: nullable.NewNullableWithValue(tid)}
		p, err := accountPatchFromBody(supplierCtx, &body)
		require.NoError(t, err)
		require.Nil(t, p.SupplierUserID)
	}
}
