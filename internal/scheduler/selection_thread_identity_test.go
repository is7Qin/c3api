// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/sdkbridge"
)

// selection.Thread 的**身份非空**守护（P5-4）。
//
// 为什么需要这条：`sdkbridge.ThreadHandle{}` 是零值 = 匿名身份。如果 Thread() 在
// "有会话"时返回零值，数据面拿到的是匿名 call——不 panic、不报错、也不会有任何
// 现有断言变红（P5-4 的对抗评审实测：把这层取值改成恒返回零值，四个包全绿）。
// 也就是说"每个 codex 请求都带上真实线程身份"这件事在今天**没有任何测试守着**，
// 只有含真实 dial 的零元用例会间接炸。本文件补的就是这条最小守护。
//
// 判据分两侧，缺一不可：
//   - 有会话 ⇒ 句柄**非零值**（身份真的取到了，而不是悄悄退化成匿名）；
//   - 无会话 ⇒ 句柄为零值（"没有会话"与"有会话但句柄丢了"必须可区分）。
//
// 放在 scheduler 包内是必须的：selection 的 session 字段不导出，只有本包能构造出
// "有会话"的那一侧。用 seam 注入池（identitypool 的测试钩子），不需要真实网络。
func TestSelectionThreadCarriesIdentityWhenSessionClaimed(t *testing.T) {
	t.Run("有会话：句柄取自该会话，且非零值", func(t *testing.T) {
		pool := newTestPool(1, testPolicy())
		require.NotNil(t, pool, "夹具前提：池必须真的建起来")

		sess := pool.Claim()
		require.NotNil(t, sess, "夹具前提：必须认领到会话")

		var s selection
		s.session = sess
		s.pool = pool

		got := s.Thread()
		require.NotEqual(t, sdkbridge.ThreadHandle{}, got,
			"有会话时 Thread() 必须是**真实身份**；返回零值等于把每个 codex 请求悄悄降级成匿名")
		require.Equal(t, sess.Thread(), got,
			"Thread() 必须原样透传会话持有的那枚句柄（不是另一枚）")
	})

	t.Run("无会话：零值（可区分于上一条）", func(t *testing.T) {
		var s selection
		require.Equal(t, sdkbridge.ThreadHandle{}, s.Thread(),
			"无会话必须是零值——正是这一条让'没有会话'与'有会话但句柄丢了'可区分")
	})
}
