// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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
