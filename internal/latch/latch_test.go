// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package latch

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTryAcquireIdempotent 锁存写入：同 (指纹, K) 重复获取被幂等吸收（返回 false）；
// 指纹或 K 任一不同 = 新身份/新代际 ⇒ 覆盖并返回 true。
func TestTryAcquireIdempotent(t *testing.T) {
	l := NewLatchStore()
	require.True(t, l.TryAcquire(1, "fp-a", 1), "首次获取")
	require.False(t, l.TryAcquire(1, "fp-a", 1), "同 (指纹,K) 幂等吸收")
	require.True(t, l.TryAcquire(1, "fp-b", 1), "指纹变更 = 新候选人")
	require.True(t, l.TryAcquire(1, "fp-b", 2), "K 前进 = 新代际")
	require.True(t, l.TryAcquire(2, "fp-a", 1), "不同账号互不影响")
}

// TestIsLatchedFencesOnFingerprintAndRevision 读侧谓词必须同时围栏**指纹**与**身份
// 代际 K**：陈旧 latch 天然不生效（正确性不得押在显式释放动作是否可靠上）。
func TestIsLatchedFencesOnFingerprintAndRevision(t *testing.T) {
	l := NewLatchStore()
	require.False(t, l.IsLatched(1, "fp-a", 1), "未锁存 ⇒ false")

	require.True(t, l.TryAcquire(1, "fp-a", 3))
	require.True(t, l.IsLatched(1, "fp-a", 3), "同 (指纹,K) ⇒ 已锁存")
	require.False(t, l.IsLatched(1, "fp-b", 3), "指纹不符 ⇒ 陈旧（不得株连新候选人）")
	require.False(t, l.IsLatched(1, "fp-a", 4), "K 前进而指纹未变 ⇒ 陈旧（围栏的 K 维度）")

	// 通配：调用方缺该维度信息时不参与比较（例如仅按账号查询）。
	require.True(t, l.IsLatched(1, "", 3), "空指纹 = 该维度通配")
	require.True(t, l.IsLatched(1, "fp-a", 0), "零 K = 该维度通配")
	require.True(t, l.IsLatched(1, "", 0), "两维皆通配 ⇒ 仅按账号")
	require.False(t, l.IsLatched(2, "", 0), "账号维度始终参与")
}

// TestClear 显式释放（recover 的唯一入口面）。
func TestClear(t *testing.T) {
	l := NewLatchStore()
	require.True(t, l.TryAcquire(1, "fp-a", 1))
	l.Clear(1)
	require.False(t, l.IsLatched(1, "fp-a", 1), "释放后未锁存")
	l.Clear(999) // 不存在亦不 panic
}

// TestClearIfFingerprintChanged 指纹变更时清旧锁存；指纹未变则保留（此时 K 围栏
// 负责让陈旧条目在**读侧**不生效，无需写侧动作）。
func TestClearIfFingerprintChanged(t *testing.T) {
	l := NewLatchStore()
	require.True(t, l.TryAcquire(1, "fp-a", 1))
	l.ClearIfFingerprintChanged(1, "fp-a")
	require.True(t, l.IsLatched(1, "fp-a", 1), "指纹未变 ⇒ 保留")
	l.ClearIfFingerprintChanged(1, "fp-b")
	require.False(t, l.IsLatched(1, "", 0), "指纹变更 ⇒ 清除")
}

// TestSnapshotIsOwnedCopy 快照是自有副本：调用方改动不影响存储（并发读安全）。
func TestSnapshotIsOwnedCopy(t *testing.T) {
	l := NewLatchStore()
	require.True(t, l.TryAcquire(1, "fp-a", 2))
	require.True(t, l.TryAcquire(2, "fp-b", 5))

	snap := l.Snapshot()
	require.Equal(t, map[LatchKey]bool{
		{AccountID: 1, Fingerprint: "fp-a", IdentityRevision: 2}: true,
		{AccountID: 2, Fingerprint: "fp-b", IdentityRevision: 5}: true,
	}, snap)

	snap[LatchKey{AccountID: 3, Fingerprint: "x", IdentityRevision: 1}] = true
	delete(snap, LatchKey{AccountID: 1, Fingerprint: "fp-a", IdentityRevision: 2})
	require.Len(t, l.Snapshot(), 2, "改动副本不得影响存储")
}

// TestConcurrentAccess 并发读写（-race 下验证无数据竞争）。
func TestConcurrentAccess(t *testing.T) {
	l := NewLatchStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := int64(i%3 + 1)
			for j := 0; j < 200; j++ {
				l.TryAcquire(id, "fp", int64(j%4+1))
				l.IsLatched(id, "fp", int64(j%4+1))
				l.Snapshot()
				if j%50 == 0 {
					l.ClearIfFingerprintChanged(id, "fp")
				}
			}
		}(i)
	}
	wg.Wait()
}
