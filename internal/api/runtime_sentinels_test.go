// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// 运行期选号哨兵的 canonical 值身份（原 internal/scheduler/errors_compat_test.go 的
// `scheduler.ErrX == api.ErrX` 那一半：删掉别名层后，等式两边只剩 api，判据因此搬到 canonical 所在包）。
//
// 为什么这条断言不能随兼容层一起消失：`errors.Is` 判的是 error 的**值身份**。这五条
// 哨兵由 scheduler 产出、由数据面（internal/proxy 的 statusFor / selectErrorMessage /
// handleSelectError）判定，两端跨包比值。任何一端把 `api.ErrX` 换成同文案的第二枚
// （`errors.New("scheduler: …")`）或 `fmt.Errorf("%w", …)`，行为断言都全绿——HTTP 状态
// 与错误文案照旧对，但 `==` 与 `%v` 已经变了，而这两者正是日志口径与将来类型化处理的
// 依据。故这里钉住四件事：
//
//  1. 逐字文案（期望值是**字面量**，不是 Error() 之间互比——那是恒真的同义反复）；
//  2. `errors.Unwrap` 必须为 nil：裸 `errors.New` 值，不是包装。第 1 条抓不到 wrap，
//     因为 `fmt.Errorf("%w", …)` 的文案与内层一模一样；
//  3. 五条互不相同：任何一条复制粘贴成另一条（`ErrNoAvailable = ErrAttemptsExhausted`）
//     都会让"值身份正确"的其他断言全绿，而两类结论的 HTTP 语义碰巧都是 429。
//
// 落点为什么是 api 而不是 scheduler：internal/api/contract_gate_test.go 的 F2 扫描面是
// api 目录**全部** .go（含 _test.go），api 的测试 import scheduler 会直接把架构环带回
// 来。canonical 的归属在 api，判据就该与它同包——这样"谁改了 api 的哨兵"第一眼就能
// 在同一个包里看到红。
//
// "全仓恰好一处声明"那一半由 internal/architecture/sentinel_single_source_gate_test.go
// 承担（它按形状扫描 internal/ + cmd/ + pkg/ 的全部非测试 .go），本文件只负责值身份。
func TestRuntimeSentinelsCanonicalIdentity(t *testing.T) {
	cases := []struct {
		name     string
		sentinel error
		wantText string
	}{
		{"ErrGroupNotFound", ErrGroupNotFound, "scheduler: group not found"},
		{"ErrFormatUnavailable", ErrFormatUnavailable, "scheduler: no account for request format"},
		{"ErrPlanNotReady", ErrPlanNotReady, "scheduler: routing plan not ready"},
		{"ErrNoAvailable", ErrNoAvailable, "scheduler: no available account"},
		{"ErrAttemptsExhausted", ErrAttemptsExhausted, "scheduler: attempt plan exhausted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.wantText, c.sentinel.Error(),
				"%s 的文案是日志/排障口径的一部分：迁移与重构都不改文案（含 scheduler: 前缀）", c.name)
			require.Nil(t, errors.Unwrap(c.sentinel),
				"%s 必须是裸 errors.New 值：fmt.Errorf(%%w) 的文案与内层相同，只有 Unwrap 抓得住包装", c.name)
			// 裸值 ⇒ errors.Is 只与它自己相等。用一枚同文案的独立 error 做反向 tripwire：
			// 若某条哨兵变成了按内容相等的错误类型，这一条会红。
			clone := errors.New(c.sentinel.Error())
			require.NotErrorIs(t, clone, c.sentinel,
				"%s 必须靠值身份判等：同文案的第二枚 error 不许 errors.Is 命中", c.name)
		})
	}

	// 反向 tripwire：五条必须互不相同（理由见函数头第 3 条）。
	for i := range cases {
		for j := i + 1; j < len(cases); j++ {
			require.True(t, cases[i].sentinel != cases[j].sentinel,
				"%s 与 %s 不许指向同一个 error：它们是不同的分类结论",
				cases[i].name, cases[j].name)
		}
	}
}
