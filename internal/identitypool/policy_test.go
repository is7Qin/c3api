// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identitypool

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/sdkbridge"
)

// TestDefaultPolicyMatchesTheSDKDefault 默认值是**契约**，而且是搬过家的那一份：
//   - 数值等于 SDK 桥历史上那个默认策略（16/48），否则"默认行为不变"只是口头承诺；
//   - 归一出口只有一个（sdk()），所以这里逐字段比 SDK 形态，而不是比自家形态；
//   - 零值语义不变：Hi == 0 表示不退休——零值**不是**默认值，两者不能混为一谈
//     （把零值读成"默认 16/48"会让"没配策略"变成"按默认策略退休"）。
//
// 这条用例同时是"所有权搬完"的可执行判据：SDK 桥不再提供默认值（那里已无该函数），
// 所以默认值只有这一处可读。
func TestDefaultPolicyMatchesTheSDKDefault(t *testing.T) {
	want := sdkbridge.RotatePolicy{WMaxLo: 16, WMaxHi: 48}
	got := DefaultPolicy().sdk()
	require.Equal(t, want.WMaxLo, got.WMaxLo, "默认策略下界（Lo 归一后 = SDK 的 WMaxLo）")
	require.Equal(t, want.WMaxHi, got.WMaxHi, "默认策略上界（Hi 归一后 = SDK 的 WMaxHi）")

	// 零值语义是**契约**：Hi == 0 ⇒ 不退休。它不是"默认值"的另一种写法。
	zero := Policy{}
	require.Zero(t, zero.Hi, "零值 = 不退休（Hi==0）；默认值必须显式取 DefaultPolicy()")
	require.Zero(t, zero.sdk().WMaxHi, "零值归一后仍是\"不退休\"，不被默认值覆盖")
	require.NotEqual(t, DefaultPolicy(), zero, "零值 ≠ 默认值")
}
