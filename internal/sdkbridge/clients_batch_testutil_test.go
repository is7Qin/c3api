// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 测试侧的**配置控制器**：把「账号全集 → 当前出口」翻译成资源指令再交给表执行。
//
// 生产里这段是 scheduler.accountRuntime.resourceBatch——传输资源表**不认识**账号
// 全集、"直连恒保留"这类策略（§3.3b-1），所以用例里必须自己给出策略，而不是让表
// 替它决定。这个 helper 与生产实现同形（保留键 = 当前出口 + 直连；表里持有但全集
// 没有的账号点名删除），只是没有真实账号行可取。

import (
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// applyUniverse 模拟一个配置批次的全部资源指令（policy 在调用侧，表只执行）。
func applyUniverse(t *testing.T, table *ClientTable, gen uint64, universe map[int64]*egress.Client) {
	t.Helper()
	table.ApplyBatch(resourceBatch(gen, universe, table.Accounts()))
}

// resourceBatch 与 scheduler.accountRuntime.resourceBatch 同形（见其注释）。
func resourceBatch(gen uint64, universe map[int64]*egress.Client, held []int64) domain.ResourceBatch {
	direct := domain.EgressResourceKey{}
	directives := make([]domain.AccountResourceDirective, 0, len(universe)+len(held))
	for id, eg := range universe {
		keep := make([]domain.EgressResourceKey, 0, 2)
		if k := egressDigest(eg); k != direct {
			keep = append(keep, k)
		}
		keep = append(keep, direct)
		directives = append(directives, domain.AccountResourceDirective{
			AccountID: id, Action: domain.ResourceRetain, Keep: keep,
		})
	}
	for _, id := range held {
		if _, ok := universe[id]; ok {
			continue
		}
		directives = append(directives, domain.AccountResourceDirective{
			AccountID: id, Action: domain.ResourceDelete,
		})
	}
	return domain.ResourceBatch{Gen: gen, Directives: directives}
}
