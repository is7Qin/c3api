// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// handleOf 取回 Selection 背后的控制面包装：私有生命周期状态（lease/session/pool）
// 只存在于包装上，测试断言这些状态时经此处显式下转型——生产代码没有这条路。
func handleOf(t *testing.T, sel *Selection) *selection {
	t.Helper()
	require.NotNil(t, sel, "selection 必须非 nil")
	h, ok := sel.Handle.(*selection)
	require.True(t, ok, "Selection.Handle 必须是控制面包装（生产装配点唯一）")
	return h
}
