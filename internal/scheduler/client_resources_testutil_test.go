// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 测试侧的**传输资源表替身**：只记录控制器下发的资源指令（scheduler.ClientResources
// 的最小面）。用它的用例关心的是"控制器算出了什么指令"，而不是表怎么执行——执行面
// 由 sdkbridge 自己的用例守住。

import (
	"sync"

	"github.com/is7qin/c3api/internal/domain"
)

type recordingResources struct {
	mu      sync.Mutex
	held    []int64
	batches []domain.ResourceBatch
}

func newRecordingResources(held ...int64) *recordingResources {
	return &recordingResources{held: held}
}

func (r *recordingResources) Accounts() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.held...)
}

func (r *recordingResources) ApplyBatch(batch domain.ResourceBatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, batch)
}

func (r *recordingResources) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func (r *recordingResources) batch(i int) domain.ResourceBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batches[i]
}

// retained 第 i 批里账号 id 的保留键（action != Retain ⇒ false）。
func (r *recordingResources) retained(i int, id int64) ([]domain.EgressResourceKey, bool) {
	for _, d := range r.batch(i).Directives {
		if d.AccountID == id && d.Action == domain.ResourceRetain {
			return d.Keep, true
		}
	}
	return nil, false
}

// deleted 第 i 批里被点名删除的账号集合。
func (r *recordingResources) deleted(i int) []int64 {
	var out []int64
	for _, d := range r.batch(i).Directives {
		if d.Action == domain.ResourceDelete {
			out = append(out, d.AccountID)
		}
	}
	return out
}

// gens 各批次的代号（按应用顺序）。
func (r *recordingResources) gens() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uint64, 0, len(r.batches))
	for _, b := range r.batches {
		out = append(out, b.Gen)
	}
	return out
}
