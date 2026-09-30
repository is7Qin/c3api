// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
)

// TestRuleJSONHelpersPropagateError 钉住 C5 修复：rule_repo 的 when/then JSON
// round-trip 不再 `_ =` 静默吞错——反序列化/marshal 失败必须显式返回错误（原
// 实现会静默丢字段并返回零值）。
func TestRuleJSONHelpersPropagateError(t *testing.T) {
	// 非法 when 值（http_status 期望 int，收到 string）→ whenFromMap 报错。
	_, err := whenFromMap(map[string]any{"http_status": "nope"})
	require.Error(t, err)

	// toDomainRule 传播同一错误（而非静默返回零值 RuleWhen）。
	_, err = toDomainRule(&ent.Rule{When: map[string]any{"http_status": "nope"}})
	require.Error(t, err)

	// marshal 失败（chan 不可序列化）→ jsonToMap 报错。
	_, err = jsonToMap(make(chan int))
	require.Error(t, err)

	// map 内含不可序列化值 → mapToJSON 报错。
	_, err = mapToJSON[domain.RuleWhen](map[string]any{"x": make(chan int)})
	require.Error(t, err)

	// 正常路径：合法值往返成功且字段保真。
	w, err := whenFromMap(map[string]any{"kind": "throttle", "http_status": float64(429)})
	require.NoError(t, err)
	require.NotNil(t, w.Kind)
	require.Equal(t, "throttle", *w.Kind)
	require.NotNil(t, w.HTTPStatus)
	require.Equal(t, 429, *w.HTTPStatus)
}
