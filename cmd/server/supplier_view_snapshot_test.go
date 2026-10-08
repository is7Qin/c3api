// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/internal/supplier"
)

var _ snapshot.Snapshot = supplierViewSnapshot{}

// fakeViewStore 视图装载持久面 fake（成功/失败注入）。
type fakeViewStore struct {
	owner map[int64]int64
	share map[int64]int
	err   error
}

func (f fakeViewStore) LoadSupplierView(context.Context) (map[int64]int64, map[int64]int, error) {
	return f.owner, f.share, f.err
}

// TestSupplierViewSnapshotWiredToRegistry ①（spec I2/§4.4/A13④）：财务视图快照
// 必须进统一快照注册表——NotReady（装载失败）经 LastError 呈现；健康轮刷新
// LastReload 且 Obs 置 loaded=true。否则「功能开了但从未装载成功」在 ops 面不可见。
func TestSupplierViewSnapshotWiredToRegistry(t *testing.T) {
	snap := proxy.NewSupplierSnapshot(time.Minute)

	// 失败轮：NotReady ⇒ Reload 返回错误（保留旧视图 fail-safe）。
	bad := supplier.NewViewLoader(supplier.ViewConfig{}, fakeViewStore{err: errors.New("db down")}, snap, nil)
	bad.SetObsProvider(func(now time.Time) any { return snap.Obs(now) })
	require.False(t, bad.LoadOnce(context.Background()), "装载失败 ⇒ LoadOnce false")

	reg := snapshot.New()
	require.NoError(t, reg.Register(supplierViewSnapshot{loader: bad}))
	errs := reg.ReloadAll(context.Background())
	require.Error(t, errs["supplier-view"], "NotReady 必须作为快照错误上报（LastError 可见）")
	require.False(t, snap.Obs(time.Now()).Loaded, "失败轮不得置 loaded")

	// 健康轮：LoadOnce 成功 ⇒ Reload 无错；obs loaded=true。
	good := supplier.NewViewLoader(supplier.ViewConfig{},
		fakeViewStore{owner: map[int64]int64{1: 7}, share: map[int64]int{7: 1000}}, snap, nil)
	good.SetObsProvider(func(now time.Time) any { return snap.Obs(now) })
	reg2 := snapshot.New()
	require.NoError(t, reg2.Register(supplierViewSnapshot{loader: good}))
	require.Empty(t, reg2.ReloadAll(context.Background()))
	obs := snap.Obs(time.Now())
	require.True(t, obs.Loaded)
	require.Equal(t, int64(1), obs.Revision)
	require.GreaterOrEqual(t, obs.StaleAgeMs, int64(0))

	status := reg2.Status()
	require.Len(t, status, 1)
	require.Equal(t, "supplier-view", status[0].Name)
	require.Nil(t, status[0].LastError)
}
