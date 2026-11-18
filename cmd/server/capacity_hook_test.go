// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/supplier"
)

// TestCapacityBlockedReachesSnapshot 钉住「唯一前向槽」的可达性。
//
// 背景：health 阶段先把 OnCapacityBlocked 回调注册给 retention worker，
// 快照要到后面的 billing 阶段才构造。回调在**运行期**（磁盘低于阈值时）才触发。
// 因此槽必须保存字段地址，而不是字段值——按值捕获的话，health 阶段拷贝到的是
// nil，磁盘满是保护会静默失效：不报错、不告警、测试全绿。
//
// 这条测试在窄化之前写，是 P4 的前置条件（spec 2026-11-18 §7.2）。
func TestCapacityBlockedReachesSnapshot(t *testing.T) {
	var field *supplier.SupplierSnapshot
	slot := supplierSnapshotSlot{p: &field}
	hook := capacityBlockedHook(slot)

	// billing 尚未写入：静默跳过，不 panic。
	require.NotPanics(t, func() { hook(true) })

	// billing 阶段写入（装配期，晚于 health 注册）。
	snap := supplier.NewSupplierSnapshot(time.Minute)
	slot.set(snap)
	require.False(t, snap.CapacityBlocked())

	// 运行期触发：必须打到同一个快照上。
	hook(true)
	require.True(t, snap.CapacityBlocked(), "前向槽必须让运行期回调看到 billing 写入的同一个快照")
	hook(false)
	require.False(t, snap.CapacityBlocked())

	// 槽本身未接线（p == nil）：仍不 panic。
	require.NotPanics(t, func() { capacityBlockedHook(supplierSnapshotSlot{})(true) })
}

// TestSupplierSnapshotSlotSeesLaterWrites 钉住「槽是引用不是快照」这一点：
// 拷贝一份槽仍然能看到后续写入，而拷贝字段值不行。
func TestSupplierSnapshotSlotSeesLaterWrites(t *testing.T) {
	var field *supplier.SupplierSnapshot
	original := supplierSnapshotSlot{p: &field}
	copied := original // 结构体按值拷贝——这正是窄结构投影的做法

	require.Nil(t, copied.get(), "写入前必须读到 nil")

	field = supplier.NewSupplierSnapshot(time.Minute)
	require.NotNil(t, copied.get(), "拷贝出来的槽仍然指向同一个字段地址")
	require.Same(t, field, copied.get())
}
