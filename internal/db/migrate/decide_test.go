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
			DBHead: RequiredVersion, BinaryHead: RequiredVersion,
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
		// 相等 = 「库就是本二进制要求的那一版」：直接拿 RequiredVersion，不再写死
		// 字面量（写死会在提高水位时把这条用例变成「落后也放行」的反向证据）。
		d := Decide(Verdict{DBHead: RequiredVersion, DBContract: 0, BinaryHead: RequiredVersion, BinaryContract: 0})
		require.Equal(t, ModeServe, d.Mode)
		require.Empty(t, d.Warnings)
		require.True(t, strings.TrimSpace(d.Reason) == "")
	})
}

// TestDecideFreshAtRequiredVersionWithoutSchemaPresenceGate 启动门**只**看水位与
// Problems：一次「刚跑完 -migrate up、表/列都在」的库，Verifier 只吐版本号、不吐
// 任何 Problems ⇒ 必须 Ready。这条钉住「不得偷偷在 Decide 里加 presence 探测」：
// 任何「再查一下表在不在」的第二次真相，都会让一个合法库因为探测自身的失败
// （超时/权限/连接池）变成 NotReady。
func TestDecideFreshAtRequiredVersionWithoutSchemaPresenceGate(t *testing.T) {
	d := Decide(Verdict{DBHead: RequiredVersion, DBContract: 0, BinaryHead: RequiredVersion, BinaryContract: 0})
	require.True(t, d.Ready(), "水位达标且无 Problems ⇒ 必须可服务（reason=%q）", d.Reason)
	require.Empty(t, d.Warnings)
}

// TestRequiredVersionIsTheBaseline 钉住「代码常量 = 本二进制真正依赖的最低
// expand 版本」这条底账（spec §2.13）：每次调高它都必须同时带上一条能被
// `-migrate up` 应用的迁移，否则所有存量部署会在升级后集体 readonly。
//
// 这里刻意不写成「RequiredVersion == Head()」的等式：head 会继续前进，下一次
// 纯 expand 迁移（没人读新结构）不应被逼成一次无意义的水位提升——水位高于库时，
// 「新二进制先发、迁移后跑」这种合法发布顺序会被判成 NotReady。反方向仍然成立：
// RequiredVersion 超过 head 表示二进制要求一个自己都建不出来的版本。
func TestRequiredVersionIsTheBaseline(t *testing.T) {
	head, _ := Head()
	require.LessOrEqual(t, RequiredVersion, head,
		"RequiredVersion 不能超过本二进制的迁移 head：否则二进制要求一个它自己都建不出来的版本")
	// 两条理由都写在这里，是因为水位是**累积**的：升到 3 的二进制同时需要 2 与 3。
	require.Equal(t, int64(3), RequiredVersion,
		"0002 把导入幂等键按凭据类型收窄（Codex/BPS 同一上游账号可并存）；"+
			"0003 = attempt_logs 写路径 + err_logs.attempt_ordinal，缺了是 42P01/42703")
}
