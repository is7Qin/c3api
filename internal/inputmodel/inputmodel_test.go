// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 必须是**外部**测试包（inputmodel_test）：同包测试要验证 repository 的别名就得
// import repository，而 repository 又 import 本包 ⇒ import cycle not allowed in test。
package inputmodel_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/inputmodel"
	"github.com/is7qin/c3api/internal/repository"
)

// TestRepositoryAliasesAreSameType 钉住"同一个类型"而不是"两份同形 struct"。
//
// 可赋值性成立当且仅当 repository 的声明是**类型别名**（带 `=`）。少了 `=` 的
// 变异会在非测试代码（repository/inputmodel_identity.go）的断言处**编译失败**，
// 而不是在这里变红——本测试是那一层的第二道可读信号，不是唯一防线。
//
// 为什么逐类型写真值而不是反射遍历：字段是 API 契约的一部分，取一个字段读回来
// 就能证明"搬包没有改变字段名/类型"（名字拼错即编译失败），这比反射清单更难
// 自欺——反射只能证明两处同构，证明不了它们没一起改错。
func TestRepositoryAliasesAreSameType(t *testing.T) {
	lq := inputmodel.ListQuery{Limit: 7, Sort: "name"}
	var lqAlias repository.ListQuery = lq
	require.Equal(t, 7, lqAlias.Limit)
	require.Equal(t, "name", lqAlias.Sort)

	from := time.Unix(1700000000, 0).UTC()
	uq := inputmodel.UsageQuery{GroupID: 3, From: &from}
	var uqAlias repository.UsageQuery = uq
	require.Equal(t, int64(3), uqAlias.GroupID)
	require.Equal(t, from, *uqAlias.From)

	status := domain.OrderStatusPending
	oq := inputmodel.OrderQuery{UserID: 11, Status: &status}
	var oqAlias repository.OrderQuery = oq
	require.Equal(t, int64(11), oqAlias.UserID)
	require.Equal(t, status, *oqAlias.Status)

	eq := inputmodel.ErrLogQuery{StatusCode: 502}
	var eqAlias repository.ErrLogQuery = eq
	require.Equal(t, 502, eqAlias.StatusCode)

	vis := domain.GroupVisibilityPublic
	gp := inputmodel.GroupPatch{Visibility: &vis}
	var gpAlias repository.GroupPatch = gp
	require.Equal(t, vis, *gpAlias.Visibility)

	baseURL := "https://upstream.example"
	tp := inputmodel.TemplatePatch{BaseURL: &baseURL}
	var tpAlias repository.TemplatePatch = tp
	require.Equal(t, baseURL, *tpAlias.BaseURL)

	ap := inputmodel.AccountPatch{TemplateID: int64Ptr(5)}
	var apAlias repository.AccountPatch = ap
	require.Equal(t, int64(5), *apAlias.TemplateID)

	role := domain.RolePlatformAdmin
	up := inputmodel.UserPatch{ID: 9, Role: &role}
	var upAlias repository.UserPatch = up
	require.Equal(t, int64(9), upAlias.ID)
	require.Equal(t, role, *upAlias.Role)

	perM := int64(1200)
	pe := inputmodel.PriceEntryManual{Model: "gpt-x", Mode: domain.PriceModeToken, InputPerM: &perM}
	var peAlias repository.PriceEntryManual = pe
	require.Equal(t, "gpt-x", peAlias.Model)
	require.Equal(t, domain.PriceModeToken, peAlias.Mode)
	require.Equal(t, perM, *peAlias.InputPerM)
}

// TestReverseAssignmentCompiles 是反方向的同一性证明：从 repository 侧构造再赋回
// canonical 类型。与上一测试合起来就是"双向可赋值"，缺任何一半都可能是碰巧同构。
func TestReverseAssignmentCompiles(t *testing.T) {
	back := repository.ListQuery{Offset: 40}
	var canonical inputmodel.ListQuery = back
	require.Equal(t, 40, canonical.Offset)
}

// TestZeroValueIsUsable 是文档性的形状断言：这些入参在无人构造时必须可零值使用
// （handler 里大量 `inputmodel.X{}` 直接返回的场景依赖它）。
func TestZeroValueIsUsable(t *testing.T) {
	var lq inputmodel.ListQuery
	require.Zero(t, lq.Limit)
	require.Nil(t, lq.Enabled)
	require.Nil(t, lq.AccountGroupID)

	var uq inputmodel.UsageQuery
	require.Nil(t, uq.From)
	require.Nil(t, uq.To)
}

func int64Ptr(v int64) *int64 { return &v }
