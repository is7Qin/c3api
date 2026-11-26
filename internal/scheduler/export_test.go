// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// export_test.go 把少量包私有实现暴露给**测试**（只在 go test 编译期存在，生产
// 二进制没有这两个符号）：P2 的 admin+PG 端到端用例在 package scheduler_test 里
// （要 import handler/service 走真实管理面 HTTP，且 handler→service→scheduler 的
// 依赖方向决定了它不能放在本包的内部测试里）。
//
// 判据不变：外部用例断言的仍是生产唯一的 digestTemplateUpstream 与唯一构造函数
// newSnapshotStatic，不是测试专用的第二份实现。

import "github.com/is7qin/c3api/internal/domain"

// DigestTemplateUpstreamForTest 暴露 digestTemplateUpstream（spec §17.2 读回口径
// 的端到端断言用）。
var DigestTemplateUpstreamForTest = digestTemplateUpstream

// SnapshotUpstreamKeyForTest 暴露唯一构造函数产出的 upstreamPathsKey 槽位。
// egress 传 nil：静态键派生不读出口 client，叶只持有该指针（测试不需要出口池）。
func SnapshotUpstreamKeyForTest(acc domain.Account, tpl *domain.Template, gids []int64) [32]byte {
	return newSnapshotStatic(acc, tpl, gids, nil, nil).key.upstreamPathsKey
}
