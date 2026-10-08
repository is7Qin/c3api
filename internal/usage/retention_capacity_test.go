// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestRetentionBlockedAlertPersistsAndClears spec §3.11：被挡分区进入**持久**告警
// ——跨巡检轮保留（首次被挡时刻保留），解除轮清空。
func TestRetentionBlockedAlertPersistsAndClears(t *testing.T) {
	oldest := time.Now().Add(-72 * time.Hour).UnixMilli()
	pm := &fakePartitionManager{blocked: []domain.BlockedPartition{{
		Name:               "usage_logs_20260101",
		UncreditedEarnRows: 5,
		UnbilledRows:       2,
		OldestUncredited:   &oldest,
		ReconSourceKeys:    1,
		ReconMismatch:      2,
		ReconOpenKeys:      3,
	}}}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30}, pm, nil)
	w.runOnce()
	st := w.Stats().(RetentionWorkerStats)
	require.Equal(t, int64(1), st.LastBlockedLogPartitions)
	require.NotNil(t, st.BlockedAlert)
	require.Len(t, st.BlockedAlert.Partitions, 1)
	require.Equal(t, "usage_logs_20260101", st.BlockedAlert.Partitions[0].Name)
	require.Equal(t, int64(5), st.BlockedAlert.Partitions[0].UncreditedEarnRows)
	require.Equal(t, oldest, st.BlockedAlert.Partitions[0].OldestUncreditedUnixMs)
	require.Equal(t, oldest, st.BlockedAlert.OldestUncreditedUnixMs)
	require.NotZero(t, st.BlockedAlert.FirstBlockedUnixMs)
	first := st.BlockedAlert.FirstBlockedUnixMs

	// 第二轮仍被挡：首次被挡时刻跨轮保留（持久告警）。
	w.runOnce()
	st = w.Stats().(RetentionWorkerStats)
	require.Equal(t, first, st.BlockedAlert.FirstBlockedUnixMs, "首次被挡时刻跨轮保留")

	// 解除：blocked 清空 ⇒ 告警清空。
	pm.blocked = nil
	w.runOnce()
	st = w.Stats().(RetentionWorkerStats)
	require.Zero(t, st.LastBlockedLogPartitions)
	require.Nil(t, st.BlockedAlert, "阻断解除后告警清空")
}

// TestRetentionCapacityEtaAndDisposition spec §3.11：磁盘采样算增长 ⇒ 容量 ETA；
// 低于下限 ⇒ 边沿处置回调（停供应商新流量），恢复回调复位；同状态不重复回调。
func TestRetentionCapacityEtaAndDisposition(t *testing.T) {
	free := int64(10 << 30) // 10 GiB
	pm := &fakePartitionManager{}
	var events []bool
	w := NewRetention(RetentionConfig{
		DiskProbe:         func() (int64, bool) { return free, true },
		DiskMinFreeBytes:  5 << 30,
		OnCapacityBlocked: func(b bool) { events = append(events, b) },
	}, pm, nil)
	w.runOnce()
	st := w.Stats().(RetentionWorkerStats)
	require.Equal(t, int64(10<<30), st.DiskFreeBytes)
	require.False(t, st.CapacityBlocked)
	require.Equal(t, int64(-1), st.DiskEtaSeconds, "首次采样无消耗观测 ⇒ ETA unknown")
	require.NotZero(t, st.DiskObservedUnixMs)
	require.Empty(t, events)

	// 降到下限之下 ⇒ 处置回调 true（边沿）。
	free = 4 << 30
	w.runOnce()
	st = w.Stats().(RetentionWorkerStats)
	require.True(t, st.CapacityBlocked)
	require.Equal(t, []bool{true}, events)

	// 同状态不重复回调。
	w.runOnce()
	require.Equal(t, []bool{true}, events, "同状态不重复回调（边沿语义）")

	// 恢复 ⇒ 回调 false。
	free = 20 << 30
	w.runOnce()
	st = w.Stats().(RetentionWorkerStats)
	require.False(t, st.CapacityBlocked)
	require.Equal(t, []bool{true, false}, events)

	// ETA = free / 消耗速率（受控时钟直接调 sampleDisk）。
	free2 := int64(8 << 30)
	w2 := NewRetention(RetentionConfig{
		DiskProbe:        func() (int64, bool) { return free2, true },
		DiskMinFreeBytes: 1,
	}, pm, nil)
	w2.sampleDisk(time.Unix(1000, 0))
	free2 = 6 << 30 // 10s 消耗 2 GiB ⇒ 0.2 GiB/s
	w2.sampleDisk(time.Unix(1010, 0))
	eta := w2.Stats().(RetentionWorkerStats).DiskEtaSeconds
	require.Equal(t, int64(float64(6<<30)/(float64(2<<30)/10.0)), eta, "ETA = free / 消耗速率")
}
