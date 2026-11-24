// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/api"
)

// 运行期哨兵的**跨包值同一**（P3c I-B 的核心判据，修正案 §D）。
//
// 为什么只能靠 `==` 钉：P3c 之前这五条各由 scheduler 铸一枚；迁移后若这里写成
// `errors.New("scheduler: …")`，数据面判到的就是另一枚值，未命中的哨兵落进
// `statusFor` 的 default（429 + "group not found"）——HTTP 语义静默改变。更隐蔽的是
// `fmt.Errorf("%w", api.ErrX)`：双向 `errors.Is` 都成立、现有 75 条端到端断言全绿、
// HTTP 映射全对，但 `==` 与 `%v` 文案都变了（P3c 修正案 §D 实测：26.9s 全绿）。
// 故断言四条，缺一不可：
//
//  1. `scheduler.ErrX == api.ErrX`：别名（同一值）才可能为真。**不许**用
//     `require.Equal` 代替——testify 对 `*errors.errorString` 按内容深比较，
//     两枚"文案相同的独立 error"会假绿（修正案 §D 给了实测复现）。
//  2. 双向 `errors.Is`：跨包判等的正面证据（数据面出错、控制面判等，反之亦然）。
//  3. 逐字文案：期望值是**字面量**，不是 `api.ErrX.Error()`。只比 alias 与 canonical
//     是恒真的同义反复；钉字面量才抓得住"canonical 被顺手改写"（也顺带守住
//     `scheduler:` 前缀这个历史日志口径）。
//  4. `errors.Unwrap` 必须为 nil：canonical 与别名都必须是裸 `errors.New` 值。
//     `fmt.Errorf("%w", …)` 的 error 文案与内层**一样**（格式串只有 `%w`），
//     所以第 3 条抓不到它——这一条才是 wrap 的唯一机械判据。
//
// 落点为什么是 scheduler（而不是 api 或 proxy）：
//   - api 侧不行：`internal/api/contract_gate_test.go` 的 F2 扫描面是 api 目录的
//     **全部** .go（含 _test.go），api 的测试 import scheduler 会直接把契约环带回来；
//   - scheduler 侧合法：本包的**生产**代码早就 import api（selection.go 的
//     `type Selection = api.Selection`），测试复用同一条已有边，不新增架构边；
//   - 就在别名旁边：这条轴的所有权在 scheduler 的兼容层，判据与声明同文件相邻，
//     改别名的人第一眼就看见判据。
func TestRuntimeSentinelsAreAPICanonical(t *testing.T) {
	cases := []struct {
		name      string
		alias     error
		canonical error
		wantText  string
	}{
		{"ErrGroupNotFound", ErrGroupNotFound, api.ErrGroupNotFound, "scheduler: group not found"},
		{"ErrFormatUnavailable", ErrFormatUnavailable, api.ErrFormatUnavailable, "scheduler: no account for request format"},
		{"ErrPlanNotReady", ErrPlanNotReady, api.ErrPlanNotReady, "scheduler: routing plan not ready"},
		{"ErrNoAvailable", ErrNoAvailable, api.ErrNoAvailable, "scheduler: no available account"},
		{"ErrAttemptsExhausted", ErrAttemptsExhausted, api.ErrAttemptsExhausted, "scheduler: attempt plan exhausted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, c.alias == c.canonical,
				"%s 必须与 api canonical 是**同一 error 值**（别名），不是同文案的第二枚，也不是 %%w 包装",
				c.name)
			require.True(t, errors.Is(c.canonical, c.alias),
				"errors.Is(api.%s, scheduler.%s) 必须为真：控制面抛出的错误要能在数据面判等",
				c.name, c.name)
			require.True(t, errors.Is(c.alias, c.canonical),
				"errors.Is(scheduler.%s, api.%s) 必须为真：数据面拿到的错误要能被控制面判等",
				c.name, c.name)
			require.Equal(t, c.wantText, c.canonical.Error(),
				"api.%s 的文案是日志/排障口径的一部分：迁移不改文案（含 scheduler: 前缀）", c.name)
			require.Equal(t, c.wantText, c.alias.Error(),
				"scheduler.%s 的文案必须来自 canonical：两套文案 = 两个 owner", c.name)
			require.Nil(t, errors.Unwrap(c.canonical),
				"api.%s 必须是裸 errors.New 值：fmt.Errorf(%%w) 的文案与内层相同，只有 Unwrap 抓得住", c.name)
			require.Nil(t, errors.Unwrap(c.alias),
				"scheduler.%s 必须是别名而非包装（errors.Is 双向成立也救不了值身份）", c.name)
		})
	}

	// 反向 tripwire：五条必须**互不相同**。表里任一条复制粘贴成另一条的值（例如
	// `ErrNoAvailable = api.ErrAttemptsExhausted`）会让上面四条全绿——同一值当然
	// 与自己相等，而两类结论的 HTTP 语义（都是 429）碰巧一样，行为测试抓不到。
	for i := range cases {
		for j := i + 1; j < len(cases); j++ {
			require.True(t, cases[i].canonical != cases[j].canonical,
				"%s 与 %s 不许指向同一个 error：五条是不同的分类结论",
				cases[i].name, cases[j].name)
		}
	}
}
