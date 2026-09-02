// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestKeyMetaProtocolConvertsIncremental A-2 红绿：key 创建/更新/轮换后 Auth
// 增量注册的 KeyMeta.ProtocolConverts 与组一致（修复前该字段恒空 → 转换方向
// 至多 60s 不可见；CreateKey 后立即请求 404 的复现根因）。
func TestKeyMetaProtocolConvertsIncremental(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "conv-k@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	converts := []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp}
	g, err := svc.CreateGroup(ctx, "conv-g", domain.GroupVisibilityPublic, nil, converts)
	require.NoError(t, err)
	require.Equal(t, converts, g.ProtocolConverts, "归一后组快照携带转换方向")

	// CreateKey：增量注册携带组转换方向
	k, err := svc.CreateKey(ctx, u.ID, "k1", g.ID, 0, 0)
	require.NoError(t, err)
	last := keys.lastMeta()
	require.NotNil(t, last, "创建后必须增量注册")
	require.Equal(t, g.ProtocolConverts, last.ProtocolConverts, "创建后快照转换方向与组一致")

	// UpdateKey（改额度）：同样携带（组预取在写库前，B1-1）
	q := int64(1000)
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, nil, &q)
	require.NoError(t, err)
	last = keys.lastMeta()
	require.Equal(t, g.ProtocolConverts, last.ProtocolConverts, "更新后快照转换方向与组一致")

	// RotateKey：新明文注册携带（旧明文删除不影响新注册字段）
	rotated, err := svc.RotateKey(ctx, u.ID, k.ID)
	require.NoError(t, err)
	last = keys.lastMeta()
	require.Equal(t, g.ProtocolConverts, last.ProtocolConverts, "轮换后快照转换方向与组一致")
	require.Equal(t, rotated.KeyRaw, keys.upserted[len(keys.upserted)-1], "最后一次注册为轮换后的新明文")
}

// TestKeyMetaProtocolConvertsEmpty off 组：无转换方向 → 快照字段空（零长度
// 切片语义——热路径 convertedRoute 对空集合零开销）。
func TestKeyMetaProtocolConvertsEmpty(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "conv-off@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := svc.CreateGroup(ctx, "off-g", domain.GroupVisibilityPublic, nil, []domain.ProtocolConvert{domain.ProtocolConvertOff})
	require.NoError(t, err)
	require.Empty(t, g.ProtocolConverts, "仅 off → 归一剔除为空")

	_, err = svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)
	last := keys.lastMeta()
	require.NotNil(t, last)
	require.Empty(t, last.ProtocolConverts, "off 组 → 快照转换方向为空")
}

// TestKeyQuotaMaxSafeIntegerBoundary Todo 3：用户端 create/update 共享 service
// 校验边界拒绝超 Number.MAX_SAFE_INTEGER（2^53−1）毫分 quota（→ ErrInvalidInput
// → 400）；上限值本身放行，0（不限）与负数维持既有语义。
func TestKeyQuotaMaxSafeIntegerBoundary(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "quota-bound@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := svc.CreateGroup(ctx, "qb-g", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)

	// create：上限放行、超限 400、负数 400（既有）
	_, err = svc.CreateKey(ctx, u.ID, "qb-top", g.ID, 0, maxKeyQuotaMillis)
	require.NoError(t, err, "quota=2^53-1 恰为前端安全整数上界，放行")
	_, err = svc.CreateKey(ctx, u.ID, "qb-over", g.ID, 0, maxKeyQuotaMillis+1)
	require.ErrorIs(t, err, ErrInvalidInput, "quota 超 Number.MAX_SAFE_INTEGER → 400")
	_, err = svc.CreateKey(ctx, u.ID, "qb-neg", g.ID, 0, -1)
	require.ErrorIs(t, err, ErrInvalidInput)

	// update：同边界（nil=不改维持放行）
	k, err := svc.CreateKey(ctx, u.ID, "qb-k", g.ID, 0, 0)
	require.NoError(t, err)
	over := maxKeyQuotaMillis + 1
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, nil, &over)
	require.ErrorIs(t, err, ErrInvalidInput, "PUT quota 超限 → 400")
	top := maxKeyQuotaMillis
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, nil, &top)
	require.NoError(t, err, "PUT quota=上限放行")
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, nil, nil)
	require.NoError(t, err, "quota nil = 不改，维持放行")
}
