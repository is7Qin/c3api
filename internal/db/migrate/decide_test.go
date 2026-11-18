// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package migrate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecideFourStates 钉住启动门的四态判定（spec §2.5）——它不碰数据库，所以
// 「水位怎么折成模式」这条规则有独立证据，不依赖 PG 用例是否跑得起来。
//
// 四个状态里最容易退化的两个：
//   - ③（库比二进制新）必须 serve：这是 k8s 回退部署的唯一救命路径，写成
//     readonly 等于"回退一次全站停服"；
//   - ①（库落后）必须 readonly 且**不是** fatal：进程要活着让 /readyz 说话。
func TestDecideFourStates(t *testing.T) {
	t.Run("database behind required version is readonly", func(t *testing.T) {
		d := Decide(Verdict{DBHead: RequiredVersion - 1, BinaryHead: 2, BinaryContract: 0})
		require.Equal(t, ModeReadonly, d.Mode)
		require.False(t, d.Ready())
		require.Contains(t, d.Reason, "-migrate up", "不可服务时必须给出下一步动作")
		require.Empty(t, d.Warnings)
	})

	t.Run("contract ahead of binary is readonly", func(t *testing.T) {
		d := Decide(Verdict{
			DBHead: 2, DBContract: 2, BinaryHead: 2, BinaryContract: 0,
			Problems: []string{"database contract head 2 is ahead of binary contract head 0"},
		})
		require.Equal(t, ModeReadonly, d.Mode)
		require.Contains(t, d.Reason, "contract head")
	})

	t.Run("checksum drift is readonly", func(t *testing.T) {
		d := Decide(Verdict{
			DBHead: 1, BinaryHead: 1,
			Problems: []string{"checksum mismatch for 0001_baseline: recorded=aa current=bb"},
		})
		require.Equal(t, ModeReadonly, d.Mode)
		require.Contains(t, d.Reason, "checksum mismatch")
	})

	t.Run("database ahead of binary serves with a warning", func(t *testing.T) {
		d := Decide(Verdict{DBHead: 3, DBContract: 0, BinaryHead: 1, BinaryContract: 0, Ahead: []int64{3}})
		require.Equal(t, ModeServe, d.Mode)
		require.True(t, d.Ready())
		require.Empty(t, d.Reason)
		require.Len(t, d.Warnings, 1)
		require.Contains(t, d.Warnings[0], "ahead")
	})

	t.Run("equal watermarks serve silently", func(t *testing.T) {
		d := Decide(Verdict{DBHead: 1, DBContract: 0, BinaryHead: 1, BinaryContract: 0})
		require.Equal(t, ModeServe, d.Mode)
		require.Empty(t, d.Warnings)
		require.True(t, strings.TrimSpace(d.Reason) == "")
	})
}

// TestRequiredVersionIsTheBaseline 钉住「代码常量 = 0001」这条底账（spec §2.13）：
// 它被改成 2 而库里还没有 0002 时，所有存量部署会在升级后集体 NotReady。改它必须
// 是一次有意的、带迁移的提交。
func TestRequiredVersionIsTheBaseline(t *testing.T) {
	head, _ := Head()
	require.LessOrEqual(t, RequiredVersion, head,
		"RequiredVersion 不能超过本二进制的迁移 head：否则二进制要求一个它自己都建不出来的版本")
	require.Equal(t, int64(1), RequiredVersion,
		"当前唯一定义 schema 的迁移是 0001_baseline；代码开始依赖新列时才改这里（spec §2.13）")
}
