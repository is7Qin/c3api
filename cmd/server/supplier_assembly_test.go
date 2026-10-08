// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/supplier"
	"github.com/is7qin/c3api/internal/worker"
)

type namedWorker struct{ name string }

func (w namedWorker) Name() string                    { return w.name }
func (w namedWorker) Start(ctx context.Context) error { return nil }
func (w namedWorker) Close(ctx context.Context) error { return nil }

var _ worker.Worker = namedWorker{}

// TestSupplierWorkersFor 供应商 worker 切片装配（spec §5.5）：
// enabled×freeze_enabled 四组合 + 注册序 thaw→credit。
func TestSupplierWorkersFor(t *testing.T) {
	thaw := namedWorker{name: "supplier-thaw"}
	credit := namedWorker{name: "supplier-credit"}

	// 关闭态：零注册（G1）。
	require.Nil(t, supplierWorkersFor(false, true, thaw, credit))

	// 启用 + 冻结：thaw 先、credit 后（注册序；反序排空 ⇒ credit 早于 thaw 关）。
	got := supplierWorkersFor(true, true, thaw, credit)
	require.Len(t, got, 2)
	require.Equal(t, "supplier-thaw", got[0].Name())
	require.Equal(t, "supplier-credit", got[1].Name())

	// 启用 + 不冻结：仅 credit（解冻 worker 不注册；存量释放由 credit 周期执行）。
	got = supplierWorkersFor(true, false, thaw, credit)
	require.Len(t, got, 1)
	require.Equal(t, "supplier-credit", got[0].Name())

	// 启用 + 冻结但 thaw 未构造（防御）：仅 credit。
	got = supplierWorkersFor(true, true, nil, credit)
	require.Len(t, got, 1)
	require.Equal(t, "supplier-credit", got[0].Name())
}

// TestSupplierWorkersImplementStatsProvider ①（spec I2）：credit/thaw/view 必须
// 满足 handler.StatsProvider（Name() + Stats() any），否则既有的类型断言会静默
// 跳过它们——/api/admin/ops/workers 看不到收益链。这里断言三者的 Stats() 返回
// 各自的具名类型（与全仓 stats 断言语义一致）。
func TestSupplierWorkersImplementStatsProvider(t *testing.T) {
	credit := supplier.NewCredit(supplier.CreditConfig{}, nil, nil)
	thaw := supplier.NewThaw(supplier.ThawConfig{}, nil, nil)
	snap := proxy.NewSupplierSnapshot(time.Minute)
	loader := supplier.NewViewLoader(supplier.ViewConfig{}, nil, snap, nil)
	loader.SetObsProvider(func(now time.Time) any { return snap.Obs(now) })

	providers := statsProviders([]worker.Worker{credit, thaw, loader}, nil)
	require.Len(t, providers, 3, "credit/thaw/view 必须全部满足 StatsProvider（否则 ops 静默缺失）")

	byName := map[string]handler.StatsProvider{}
	for _, p := range providers {
		byName[p.Name()] = p
	}
	require.Contains(t, byName, "supplier-credit")
	require.Contains(t, byName, "supplier-thaw")
	require.Contains(t, byName, "supplier-view")

	_, ok := byName["supplier-credit"].Stats().(supplier.CreditStats)
	require.True(t, ok, "supplier-credit Stats 必须是 CreditStats")
	_, ok = byName["supplier-thaw"].Stats().(supplier.ThawStats)
	require.True(t, ok, "supplier-thaw Stats 必须是 ThawStats")
	viewStats, ok := byName["supplier-view"].Stats().(supplier.ViewLoadStats)
	require.True(t, ok, "supplier-view Stats 必须是 ViewLoadStats")
	require.NotNil(t, viewStats.Snapshot, "view Stats 必须携带三态 Obs（装配期注入）")

	// 端到端：/api/admin/ops/workers 必须能看到这三个 worker（I2 的可见性目标）。
	api := handler.New(nil, handler.OpsOptions{Workers: providers})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body handler.WorkersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	seen := map[string]bool{}
	for _, w := range body.Workers {
		seen[w.Name] = true
	}
	for _, name := range []string{"supplier-credit", "supplier-thaw", "supplier-view"} {
		require.True(t, seen[name], "/ops/workers 必须包含 %s", name)
	}
}
