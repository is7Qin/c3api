// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeThawStore ThawStore fake（按序返回删除数 + 固定桶观测）。
type fakeThawStore struct {
	dueSeq  []int
	dueIdx  int
	dueErr  error
	snap    ThawBucketSnapshot
	snapErr error
}

func (f *fakeThawStore) ThawDueChunks(context.Context, int) (int, error) {
	if f.dueErr != nil {
		return 0, f.dueErr
	}
	if f.dueIdx >= len(f.dueSeq) {
		return 0, nil
	}
	n := f.dueSeq[f.dueIdx]
	f.dueIdx++
	return n, nil
}

func (f *fakeThawStore) ThawBucketStats(context.Context) (ThawBucketSnapshot, error) {
	return f.snap, f.snapErr
}

// TestThawWorkerMetrics spec I2/§7.2/G7：每轮刷新桶物理观测（bucket_rows/
// bucket_lag/overdue 行与金额）；速率窗口给 5min 成功删除行/s；ETA = 到期桶数/速率，
// 速率 0 ⇒ -1；观测查询失败 ⇒ bucket_stale（保留旧值不冒充新鲜）。
func TestThawWorkerMetrics(t *testing.T) {
	fs := &fakeThawStore{
		dueSeq: []int{5},
		snap:   ThawBucketSnapshot{BucketRows: 100, BucketLagSeconds: 3600, OverdueRows: 10, OverdueAmount: 1234},
	}
	w := NewThaw(ThawConfig{}, fs, nil)
	w.runOnce(context.Background())
	st := w.Stats().(ThawStats)
	require.Equal(t, int64(5), st.DeletedTotal)
	require.Equal(t, int64(1), st.Cycles)
	require.Equal(t, int64(100), st.BucketRows)
	require.Equal(t, int64(3600), st.BucketLagSeconds)
	require.Equal(t, int64(10), st.OverdueRows)
	require.Equal(t, int64(1234), st.OverdueAmount)
	require.False(t, st.BucketStale)

	// 速率窗口（独立 worker，受控时钟）：5 行 / 1s ⇒ 5 行/s；到期 10 ⇒ ETA=2s。
	wr := NewThaw(ThawConfig{}, fs, nil)
	wr.overdueRows.Store(10)
	wr.recordRate(time.Unix(1000, 0), 0)
	wr.recordRate(time.Unix(1001, 0), 5)
	str := wr.Stats().(ThawStats)
	require.InDelta(t, 5.0, str.ThawRatePerSec, 1e-9)
	require.Equal(t, int64(2), str.RecoveryEtaSeconds)

	// 速率 0（窗口无进展）⇒ ETA unknown（-1）。
	wr2 := NewThaw(ThawConfig{}, fs, nil)
	wr2.recordRate(time.Unix(1000, 0), 0)
	wr2.recordRate(time.Unix(1001, 0), 0)
	require.Equal(t, int64(-1), wr2.Stats().(ThawStats).RecoveryEtaSeconds, "速率 0 ⇒ ETA unknown")

	// 观测查询失败 ⇒ stale（保留旧值）；解冻失败轮同样刷新（不静默）。
	fsErr := &fakeThawStore{snapErr: errors.New("boom")}
	w2 := NewThaw(ThawConfig{}, fsErr, nil)
	w2.runOnce(context.Background())
	st2 := w2.Stats().(ThawStats)
	require.True(t, st2.BucketStale, "观测查询失败必须标 stale")
	require.Equal(t, 0.0, st2.ThawRatePerSec)
	require.Equal(t, int64(-1), st2.RecoveryEtaSeconds)

	// 解冻删除失败（dueErr）：观测仍刷新（bucketStale=false），deleted 不推进。
	fsDue := &fakeThawStore{dueErr: errors.New("thaw boom"), snap: ThawBucketSnapshot{BucketRows: 7}}
	w3 := NewThaw(ThawConfig{}, fsDue, nil)
	w3.runOnce(context.Background())
	st3 := w3.Stats().(ThawStats)
	require.Equal(t, int64(0), st3.DeletedTotal)
	require.Equal(t, int64(7), st3.BucketRows, "解冻失败轮仍刷新桶观测")
	require.False(t, st3.BucketStale)
}
