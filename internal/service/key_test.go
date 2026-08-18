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
