// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetContains_NilDefense(t *testing.T) {
	// nil 集合不 panic，返回 false
	require.NotPanics(t, func() {
		var s *intSet
		require.False(t, s.contains(nil))
		require.False(t, s.contains(intPtr(1)))
	})
	require.NotPanics(t, func() {
		var s *stringSet
		require.False(t, s.contains("a"))
		require.False(t, s.contains(""))
	})
	require.NotPanics(t, func() {
		var s *substringSet
		require.False(t, s.contains("a"))
		require.False(t, s.contains(""))
	})
	// nil 输入构造返回 nil，不 panic
	require.NotPanics(t, func() {
		s, err := newIntSet(nil)
		require.NoError(t, err)
		require.Nil(t, s)
		require.False(t, s.contains(intPtr(500)))
	})
	require.NotPanics(t, func() {
		s, err := newStringSet(nil)
		require.NoError(t, err)
		require.Nil(t, s)
		require.False(t, s.contains("a"))
	})
	require.NotPanics(t, func() {
		s, err := newSubstringSet(nil)
		require.NoError(t, err)
		require.Nil(t, s)
		require.False(t, s.contains("a"))
	})
	require.NotPanics(t, func() {
		s, err := newIntSet([]int{})
		require.NoError(t, err)
		require.Nil(t, s)
	})
	// intSet 含值时，查询 nil 指针不 panic
	s, err := newIntSet([]int{500})
	require.NoError(t, err)
	require.NotPanics(t, func() {
		require.False(t, s.contains(nil))
	})
}

func TestSetSmallExpand(t *testing.T) {
	// 1 元素：vals 分支
	s1, err := newIntSet([]int{500})
	require.NoError(t, err)
	require.NotNil(t, s1)
	require.Nil(t, s1.m, "len 1 走 vals 不建 map")
	require.True(t, s1.contains(intPtr(500)))
	require.False(t, s1.contains(intPtr(501)))
	require.False(t, s1.contains(nil))

	// 2 元素：vals 分支
	s2, err := newIntSet([]int{500, 502})
	require.NoError(t, err)
	require.Nil(t, s2.m, "len 2 走 vals")
	require.True(t, s2.contains(intPtr(500)))
	require.True(t, s2.contains(intPtr(502)))
	require.False(t, s2.contains(intPtr(503)))

	// 4 元素：阈值边界 vals 分支
	s4, err := newIntSet([]int{400, 401, 500, 502})
	require.NoError(t, err)
	require.Nil(t, s4.m, "len 4 阈值内走 vals")
	for _, v := range []int{400, 401, 500, 502} {
		require.True(t, s4.contains(intPtr(v)), "hit %d", v)
	}
	require.False(t, s4.contains(intPtr(503)))

	// >4 元素：map 分支
	s5, err := newIntSet([]int{400, 401, 500, 502, 503})
	require.NoError(t, err)
	require.NotNil(t, s5.m, "len 5 走 map")
	require.True(t, s5.contains(intPtr(503)))
	require.False(t, s5.contains(intPtr(504)))

	// stringSet 同阈值
	str1, _ := newStringSet([]string{"a"})
	require.Nil(t, str1.m)
	require.True(t, str1.contains("a"))
	require.False(t, str1.contains("b"))

	str4, _ := newStringSet([]string{"a", "b", "c", "d"})
	require.Nil(t, str4.m, "len 4 走 vals")
	require.True(t, str4.contains("d"))
	require.False(t, str4.contains("e"))

	str5, _ := newStringSet([]string{"a", "b", "c", "d", "e"})
	require.NotNil(t, str5.m, "len 5 走 map")
	require.True(t, str5.contains("e"))
	require.False(t, str5.contains("f"))

	// substringSet 恒 vals，无 map 分支
	sub1, _ := newSubstringSet([]string{"overload"})
	require.True(t, sub1.contains("system overload"))
	require.False(t, sub1.contains("busy"))
	sub4, _ := newSubstringSet([]string{"a", "b", "c", "d"})
	require.True(t, sub4.contains("x c y"))
	require.False(t, sub4.contains("z"))
}
