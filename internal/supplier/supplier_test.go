// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// TestEffectiveFreezeHours I5 冻结参数四组合。
func TestEffectiveFreezeHours(t *testing.T) {
	zero, four := 0, 4
	require.Equal(t, 0, EffectiveFreezeHours(false, &four, 24), "全局关闭优先 ⇒ 0（忽略覆盖）")
	require.Equal(t, 0, EffectiveFreezeHours(false, nil, 24), "全局关闭 + 无覆盖 ⇒ 0")
	require.Equal(t, 24, EffectiveFreezeHours(true, nil, 24), "开 + 无覆盖 ⇒ 全局默认")
	require.Equal(t, 4, EffectiveFreezeHours(true, &four, 24), "开 + 覆盖 >0")
	require.Equal(t, 0, EffectiveFreezeHours(true, &zero, 24), "开 + 覆盖 0 ⇒ 不冻结")
}

// TestBucketAvailableAt 桶时刻（ceil 到网格 + H×3600；纯整数）。
func TestBucketAvailableAt(t *testing.T) {
	g := 2 * time.Hour
	// credited 恰在网格点：t=7200（02:00）⇒ ceil=7200；+ 24h=86400 ⇒ +86400。
	got := BucketAvailableAt(time.Unix(7200, 0), g, 24)
	require.Equal(t, int64(7200+24*3600), got.Unix())
	// credited 偏离网格（+1s）：ceil 抬到下一网格 14400；+24h。
	got = BucketAvailableAt(time.Unix(7201, 0), g, 24)
	require.Equal(t, int64(14400+24*3600), got.Unix())
	// freeze_hours=0：仅 ceil 到网格。
	got = BucketAvailableAt(time.Unix(1, 0), g, 0)
	require.Equal(t, int64(7200), got.Unix())
}

// TestMaxActiveBuckets G7 第一层紧上界：默认 24/2h ⇒ 13。
func TestMaxActiveBuckets(t *testing.T) {
	require.Equal(t, int64(13), MaxActiveBuckets(2*time.Hour, 24), "默认 H=24,g=2h ⇒ ceil(12)+1=13")
	require.Equal(t, int64(64), MaxActiveBuckets(2*time.Hour, 126), "126h/2h ⇒ 64")
	require.Equal(t, int64(65), MaxActiveBuckets(2*time.Hour, 127), "127h/2h ⇒ 65（越 64 上界）")
}

// TestAggregateBatch 聚合/守恒/封账/锁序。
func TestAggregateBatch(t *testing.T) {
	day1 := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	rows := []BatchRow{
		{ID: 1, UID: 200, Cost: 1000, Earn: 100, CreatedAt: day1},
		{ID: 2, UID: 100, Cost: 2000, Earn: 200, CreatedAt: day1},
		{ID: 3, UID: 200, Cost: 3000, Earn: 300, CreatedAt: day2},
	}
	freeze := map[int64]int{200: 24, 100: 0}
	totals, recon, err := AggregateBatch(rows, freeze)
	require.NoError(t, err)
	// uid 升序。
	require.Len(t, totals, 2)
	require.Equal(t, int64(100), totals[0].UID)
	require.Equal(t, int64(200), totals[0].Earn, "uid100 earn=200")
	require.Equal(t, int64(200), totals[0].ToAvailable, "100 不冻结 ⇒ 全 available")
	require.Equal(t, int64(0), totals[0].ToFrozen)
	require.Equal(t, int64(200), totals[1].UID)
	require.Equal(t, int64(400), totals[1].Earn, "uid200 earn=100+300=400")
	require.Equal(t, int64(400), totals[1].ToFrozen, "200 冻结 ⇒ 全 frozen")
	require.Equal(t, int64(0), totals[1].ToAvailable)
	// 封账：uid200 跨 2 源日 + uid100 单日 ⇒ 3 行，按 uid 升序（100 在前）。
	require.Len(t, recon, 3)
	require.Equal(t, int64(100), recon[0].UID)
	require.Equal(t, int64(200), recon[1].UID)
	require.Equal(t, int64(200), recon[2].UID)
	require.True(t, recon[1].SourceDay.Before(recon[2].SourceDay), "uid200 两源日升序")
}

// TestAggregateBatchConservation 守恒违例（构造 impossible？守恒由构造保证，
// 故测溢出与负值失败闭合 + 重复 id）。
func TestAggregateBatchFailClosed(t *testing.T) {
	_, _, err := AggregateBatch([]BatchRow{{ID: 1, UID: 1, Earn: 1}, {ID: 1, UID: 1, Earn: 1}}, nil)
	require.Error(t, err, "重复 id ⇒ 错误")

	_, _, err = AggregateBatch([]BatchRow{{ID: 1, UID: 1, Earn: math.MaxInt64}, {ID: 2, UID: 1, Earn: 1}}, nil)
	require.Error(t, err, "溢出 ⇒ 失败闭合（不静默钳制）")

	_, _, err = AggregateBatch([]BatchRow{{ID: 1, UID: 1, Earn: -1}}, nil)
	require.Error(t, err, "负 earn ⇒ 错误")
}

// TestAddCheckedOverflowAndUnderflow 解冻聚合/记账守恒复用的溢出检查（M1）：
// 越界 ⇒ 失败闭合（不静默钳制）；边界内 ⇒ 正常相加。
func TestAddCheckedOverflowAndUnderflow(t *testing.T) {
	if _, err := AddChecked(math.MaxInt64, 1); err == nil {
		t.Fatal("MaxInt64 + 1 必须失败闭合")
	}
	if _, err := AddChecked(math.MinInt64, -1); err == nil {
		t.Fatal("MinInt64 - 1 必须失败闭合")
	}
	got, err := AddChecked(100, 200)
	require.NoError(t, err)
	require.Equal(t, int64(300), got)
	// 零/负增量不误报（delta 非负由调用方断言）。
	got, err = AddChecked(math.MaxInt64, 0)
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), got)
}

// TestIsRetryableTxErr 死锁/锁不可用可重试。
func TestIsRetryableTxErr(t *testing.T) {
	require.True(t, IsRetryableTxErr(&pgconn.PgError{Code: "40P01"}))
	require.True(t, IsRetryableTxErr(&pgconn.PgError{Code: "55P03"}))
	require.False(t, IsRetryableTxErr(&pgconn.PgError{Code: "23505"}))
	require.False(t, IsRetryableTxErr(errors.New("boom")))
	require.False(t, IsRetryableTxErr(nil))
}

// ---- worker cycle（fake store） ----

type fakeStore struct {
	batches     [][]BatchRow
	fetchIdx    int
	applied     int
	appliedUIDs [][]int64
	applyErrs   []error // 依次返回的错误（nil = 成功）
	freeze      map[int64]int
	released    int
	releaseN    int
	releaseSeq  []int // 非空时按序返回（用于「最终收敛 / 预算边界」断言）
	head        int64
	fullRows    int64
	fullSum     int64
}

func (f *fakeStore) FetchCreditBatch(ctx context.Context, limit int) ([]BatchRow, error) {
	if f.fetchIdx >= len(f.batches) {
		return nil, nil
	}
	b := f.batches[f.fetchIdx]
	f.fetchIdx++
	return b, nil
}
func (f *fakeStore) FreezeHoursByUID(ctx context.Context, uids []int64) (map[int64]int, error) {
	return f.freeze, nil
}
func (f *fakeStore) ApplyCreditTx(ctx context.Context, batch []BatchRow, freeze map[int64]int) error {
	f.applied++
	f.appliedUIDs = append(f.appliedUIDs, uniqueUIDs(batch))
	if len(f.applyErrs) > 0 {
		err := f.applyErrs[0]
		f.applyErrs = f.applyErrs[1:]
		return err
	}
	return nil
}
func (f *fakeStore) ReleaseFrozenNoWait(ctx context.Context, limit int) (int, error) {
	f.released++
	if len(f.releaseSeq) > 0 {
		n := f.releaseSeq[0]
		f.releaseSeq = f.releaseSeq[1:]
		return n, nil
	}
	return f.releaseN, nil
}
func (f *fakeStore) CreditLagHead(ctx context.Context) (int64, error) { return f.head, nil }
func (f *fakeStore) CreditLagFull(ctx context.Context) (int64, int64, error) {
	return f.fullRows, f.fullSum, nil
}

// TestCreditWorkerRunOnce 单周期：取批 → apply（含去重 uid）→ lag。
func TestCreditWorkerRunOnce(t *testing.T) {
	fs := &fakeStore{
		batches: [][]BatchRow{{
			{ID: 1, UID: 200, Cost: 10, Earn: 1},
			{ID: 2, UID: 100, Cost: 10, Earn: 1},
			{ID: 3, UID: 200, Cost: 10, Earn: 1},
		}},
		freeze:   map[int64]int{200: 24, 100: 0},
		head:     3,
		fullRows: 3,
		fullSum:  3,
	}
	w := NewCredit(CreditConfig{FreezeEnabled: true, LagFullEvery: 1}, fs, nil)
	w.runOnce(context.Background())
	require.Equal(t, 1, fs.applied)
	require.Equal(t, []int64{200, 100}, fs.appliedUIDs[0], "uid 去重（顺序 = 出现序）")
	require.Equal(t, int64(3), w.Stats().LagHead)
	require.Equal(t, int64(3), w.Stats().LagFull)
	require.Equal(t, 0, fs.released, "freeze_enabled=true ⇒ 不释放存量")
}

// TestCreditWorkerFreezeDisabledReleases freeze_enabled=false ⇒ 每周期释放存量
// （即使无批）。
func TestCreditWorkerFreezeDisabledReleases(t *testing.T) {
	fs := &fakeStore{releaseN: 2}
	w := NewCredit(CreditConfig{FreezeEnabled: false}, fs, nil)
	w.runOnce(context.Background())
	require.Equal(t, 1, fs.released)
	require.Equal(t, 0, fs.applied, "空批 ⇒ 不 apply")
}

// TestCreditWorkerRetryOnDeadlock 40P01 有界重试后成功。
func TestCreditWorkerRetryOnDeadlock(t *testing.T) {
	fs := &fakeStore{
		batches:   [][]BatchRow{{{ID: 1, UID: 1, Cost: 10, Earn: 1}}},
		freeze:    map[int64]int{1: 0},
		applyErrs: []error{&pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "40P01"}},
	}
	w := NewCredit(CreditConfig{FreezeEnabled: true}, fs, nil)
	w.runOnce(context.Background())
	require.Equal(t, 3, fs.applied, "两次可重试失败 + 一次成功")
}

// TestCreditWorkerDrain Close 排空到 backlog==0。
func TestCreditWorkerDrain(t *testing.T) {
	fs := &fakeStore{
		batches: [][]BatchRow{
			{{ID: 1, UID: 1, Cost: 10, Earn: 1}},
			{{ID: 2, UID: 1, Cost: 10, Earn: 1}},
			nil, // 收敛
		},
		freeze: map[int64]int{1: 0},
	}
	w := NewCredit(CreditConfig{FreezeEnabled: true, DrainBudget: time.Second}, fs, nil)
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, 2, fs.applied)
}

// TestCreditWorkerCloseDrainsFrozenChunks 空 backlog + 存量 chunks ⇒ Close 仍排空
// 冻结桶（§5.4/§5.5：freeze_enabled=false 时不得因 backlog==0 提前退出）。
func TestCreditWorkerCloseDrainsFrozenChunks(t *testing.T) {
	fs := &fakeStore{releaseSeq: []int{3, 2, 0}}
	w := NewCredit(CreditConfig{FreezeEnabled: false, DrainBudget: time.Second}, fs, nil)
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, 3, fs.released, "Close 须每轮释放直到 released==0")
	require.Equal(t, 0, fs.applied, "空 backlog ⇒ 不 apply")
}

// TestCreditWorkerCloseBudgetBounded 总预算到期必须退出（即使存量仍在释放）。
func TestCreditWorkerCloseBudgetBounded(t *testing.T) {
	fs := &fakeStore{releaseN: 1} // 永远释放 1 行（模拟持续存量）
	w := NewCredit(CreditConfig{FreezeEnabled: false, DrainBudget: 50 * time.Millisecond}, fs, nil)
	start := time.Now()
	require.NoError(t, w.Close(context.Background()))
	require.Less(t, time.Since(start), 5*time.Second, "总预算到期必须退出")
	require.Greater(t, fs.released, 0)
}
