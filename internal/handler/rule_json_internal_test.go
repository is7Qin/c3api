// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestMustJSONMapPropagatesError 钉住 C5 修复：rule 契约转换（when/then）的 JSON
// round-trip 不再静默吞错——不可序列化值必须显式暴露（panic），而非静默返回空
// map 让字段丢失。
func TestMustJSONMapPropagatesError(t *testing.T) {
	require.Panics(t, func() { _ = mustJSONMap(make(chan int)) })

	// 正常领域结构往返非空且不 panic（字段保真）。
	kind := "throttle"
	require.NotPanics(t, func() {
		m := whenToAPI(domain.RuleWhen{Kind: &kind})
		require.Equal(t, "throttle", m["kind"])
	})
}
