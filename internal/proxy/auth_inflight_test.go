// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// /api/admin/users-top 数据源（spec 2026-08-14 P2-3）：Auth.InFlightUsers 只读
// 访问器——acquire/release 后反映在途数（gateSnapshot 原子换入，store.Load()
// 零锁遍历）；含 0 值条目（过滤由 handler 做）；跨 reload 在途继承不丢。
func TestAuthInFlightUsersAcquireRelease(t *testing.T) {
	loader := &mutKeyLoader{keys: map[string]domain.KeyMeta{
		"k1": activeKeyWithMax(1, 1, 10), // user 1, user 上限 10
		"k2": activeKeyWithMax(2, 2, 10), // user 2
		"k3": activeKeyWithMax(3, 3, 10), // user 3
	}}
	a := NewAuth(loader, noopUserLoader{}, nil)
	require.NoError(t, a.Reload(context.Background()))

	m1 := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 10, KeyMaxConc: 10,
		KeyStatus: domain.KeyStatusActive, UserStatus: domain.UserStatusActive}

	// 初始无在途：全部 0（访问器含 0 值条目——过滤由 handler 做）。
	got := a.InFlightUsers()
	require.Len(t, got, 3, "快照 3 用户全在 map（含 0 在途）")
	require.Equal(t, int64(0), got[1])

	// user 1 并发 acquire 2 次（key 上限 10 不阻）
	_, ok1 := a.Acquire(m1)
	require.True(t, ok1)
	_, ok2 := a.Acquire(m1)
	require.True(t, ok2)
	got = a.InFlightUsers()
	require.Equal(t, int64(2), got[1], "acquire×2 后在途=2")
	require.Equal(t, int64(0), got[2], "未 acquire 用户 0")
	require.Equal(t, int64(0), got[3])

	// release 1 次 → 在途回落
	a.Release(m1, 1)
	got = a.InFlightUsers()
	require.Equal(t, int64(1), got[1], "release×1 后在途=1")

	// 全部 release → 0（非删除条目——快照在途继承，users map 恒含快照用户）
	a.Release(m1, 1)
	got = a.InFlightUsers()
	require.Equal(t, int64(0), got[1], "全部 release 后在途=0")

	// 跨 reload 在途继承（acquire 后再 Reload，新快照继承旧值）
	_, ok3 := a.Acquire(m1)
	require.True(t, ok3)
	require.NoError(t, a.Reload(context.Background()))
	got = a.InFlightUsers()
	require.Equal(t, int64(1), got[1], "reload 后新快照继承在途=1")
}

// activeKeyWithMax 带并发上限的启用态 KeyMeta（UserMaxConc > 0——限流 + 计数；
// activeKey 的 0 = 不限并发但仍计数——计数与限流解耦，排行数据源，spec
// 2026-08-15）。
func activeKeyWithMax(keyID, userID int64, maxConc int) domain.KeyMeta {
	m := activeKey(keyID, userID, 0)
	m.UserMaxConc = maxConc
	return m
}
