// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestSelectBaseURLPriority 钉死 Select 的 base_url 优先级（账号级 > 模板级）：
// 账号级非空覆盖模板级；账号级 nil/空串回退模板级（热路径仅 nil 检查 +
// 字符串比较，零分配——用户裁决 2026-08-14）。断言 Selection.BaseURL。
func TestSelectBaseURLPriority(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"}) // BaseURL "https://u/v1"

	t.Run("账号级非空覆盖模板级", func(t *testing.T) {
		a := acc(1, tplA, 4)
		a.BaseURL = strPtrT("https://acc.example.com")
		s := newTestScheduler(t, []*domain.Account{a})
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, "https://acc.example.com", sel.BaseURL, "账号级非空必须覆盖模板级")
		sel.Release()
	})

	t.Run("账号级 nil 回退模板级", func(t *testing.T) {
		a := acc(2, tplA, 4) // BaseURL nil
		s := newTestScheduler(t, []*domain.Account{a})
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, "https://u/v1", sel.BaseURL, "账号级 nil → 继承模板 base_url")
		sel.Release()
	})

	t.Run("账号级空串回退模板级", func(t *testing.T) {
		a := acc(3, tplA, 4)
		a.BaseURL = strPtrT("")
		s := newTestScheduler(t, []*domain.Account{a})
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, "https://u/v1", sel.BaseURL, "账号级空串 → 继承模板 base_url")
		sel.Release()
	})
}
