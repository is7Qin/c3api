// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"fmt"
	"sync"
	"testing"
)

// TestTurnStateSlotsConcurrentDistinctKeys：并发写**不同键**不得丢写（oracle 评审
// 要求补的覆盖面：外层表 CAS 重试路径）。每个 goroutine 只写自己的键，结束后逐个
// 校验存在且值正确。
func TestTurnStateSlotsConcurrentDistinctKeys(t *testing.T) {
	const n = 64
	var s turnStateSlots
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("inst|sess|thread-%d", i)
			s.store(key, fmt.Sprintf("T%d", i))
		}()
	}
	wg.Wait()
	for i := range n {
		key := fmt.Sprintf("inst|sess|thread-%d", i)
		if got, want := s.load(key), fmt.Sprintf("T%d", i); got != want {
			t.Fatalf("load(%q) = %q, 期望 %q（并发插入丢写）", key, got, want)
		}
	}
}

// TestTurnStateSlotsClearAndRewrite：清除保留值单元，同键再写可复用（不复制整表）。
func TestTurnStateSlotsClearAndRewrite(t *testing.T) {
	var s turnStateSlots
	s.store("k", "T1")
	if got := s.load("k"); got != "T1" {
		t.Fatalf("load = %q, 期望 T1", got)
	}
	s.store("k", "")
	if got := s.load("k"); got != "" {
		t.Fatalf("清除后 load = %q, 期望空", got)
	}
	s.store("k", "T2")
	if got := s.load("k"); got != "T2" {
		t.Fatalf("再次写入后 load = %q, 期望 T2", got)
	}
	// 幂等：键不存在时清除是 no-op，且不应建出条目。
	s.store("missing", "")
	if m := s.m.Load(); m == nil || len(*m) != 1 {
		t.Fatalf("表大小 = %v, 期望 1（no-op 清除不得建条目）", m)
	}
}

// TestTurnStateSlotsCompactionDropsEmptyShells：插入新键时压缩掉空壳（已结束的轮），
// 但**保留**有值的会话——否则压缩会把活跃 ts 一起扔掉。
func TestTurnStateSlotsCompactionDropsEmptyShells(t *testing.T) {
	var s turnStateSlots
	// 造到阈值以上：偶数键有值（活跃会话），奇数键清空（空壳）。
	for i := range turnStateSlotCompactAt + 8 {
		key := fmt.Sprintf("fill-%d", i)
		s.store(key, fmt.Sprintf("T%d", i))
		if i%2 == 1 {
			s.store(key, "")
		}
	}
	// 再插一个新键：这次写入必然触发压缩（len > 阈值）。
	s.store("trigger-compaction", "Tnew")

	m := s.m.Load()
	if m == nil {
		t.Fatal("表未发布")
	}
	if len(*m) > turnStateSlotCompactAt {
		t.Fatalf("压缩后表大小 = %d, 期望 ≤ %d", len(*m), turnStateSlotCompactAt)
	}
	// 有值的会话一个都不能少。
	for i := 0; i < turnStateSlotCompactAt+8; i += 2 {
		key := fmt.Sprintf("fill-%d", i)
		if got, want := s.load(key), fmt.Sprintf("T%d", i); got != want {
			t.Fatalf("压缩后 load(%q) = %q, 期望 %q（活跃值被误删）", key, got, want)
		}
	}
	// 空壳已回收（不是"留着空串"，而是键本身不在表里）。
	if _, present := (*m)["fill-1"]; present {
		t.Fatal("空壳 fill-1 仍在表里（压缩没生效）")
	}
	if got := s.load("trigger-compaction"); got != "Tnew" {
		t.Fatalf("新键值 = %q, 期望 Tnew", got)
	}
}
