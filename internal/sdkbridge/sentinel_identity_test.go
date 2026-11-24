// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/identityfence"
)

// 身份哨兵的**跨包互认**（P3c I-A 的核心价值）：sdkbridge 的三个导出名必须是
// identityfence canonical 的同一 error 值——不是"文案一样的第二枚"。
//
// 为什么这件事只能靠断言值身份来钉：`errors.Is` 判的是 error 的值身份。P3c 之前
// 两边各铸一枚独立哨兵（`sdkbridge:` 前缀），于是
// `errors.Is(identityfence.ErrMissingCandidateFingerprint, sdkbridge.ErrMissingCandidateFingerprint)`
// 恒 false：上游（rulepersist / scheduler）已经分类成"缺身份"的判决，落到失效链
// 这里会被 isTransientFailure 判成"可重试"，静默地走了另一条路。
//
// 断言三条，缺一不可：
//  1. 直接值比较 `==` 为真：别名（同一指针）才可能，fmt.Errorf 的 wrap 会为假；
//  2. 双向 `errors.Is` 为真：跨包判等的正面证据（本单要的行为就在这里）；
//  3. 文案相等：别名不会带来第二套文案（消息文本仍归 canonical 所有）。
//
// 放在 sdkbridge 侧而不是 identityfence 侧是编译约束：identityfence 的同包测试
// （package identityfence）import sdkbridge 会构成 import 环（sdkbridge 生产代码
// import identityfence）。本方向的 import 是已有的生产边，不新增架构边。
func TestIdentitySentinelsAreSharedWithIdentityfence(t *testing.T) {
	cases := []struct {
		name      string
		alias     error
		canonical error
	}{
		{"ErrMissingCandidateFingerprint", ErrMissingCandidateFingerprint, identityfence.ErrMissingCandidateFingerprint},
		{"ErrCandidateFingerprintMismatch", ErrCandidateFingerprintMismatch, identityfence.ErrCandidateFingerprintMismatch},
		{"ErrMissingExpectedRevision", ErrMissingExpectedRevision, identityfence.ErrMissingExpectedRevision},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, c.alias == c.canonical,
				"%s 必须与 identityfence canonical 是**同一 error 值**（别名），而不是 wrap/重建",
				c.name)
			require.True(t, errors.Is(c.canonical, c.alias),
				"errors.Is(identityfence.%s, sdkbridge.%s) 必须为真：上游分类的错误要能在本包判等",
				c.name, c.name)
			require.True(t, errors.Is(c.alias, c.canonical),
				"errors.Is(sdkbridge.%s, identityfence.%s) 必须为真：本包抛出的错误要能被上游判等",
				c.name, c.name)
			require.Equal(t, c.canonical.Error(), c.alias.Error(),
				"%s 的文案归 canonical 所有：两套文案 = 两个 owner", c.name)
		})
	}
}
