// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSnapshotObsJSONKeys 钉死 ops 面 /ops/workers 的 snapshot 子对象线上形状：
// 键必须是 snake_case（loaded / revision / last_success_unix_ms / stale_age_ms）。
//
// 回归面：struct 少一个 json tag 就会退回 Go 字段名（`Loaded`），前端 ops.statField.*
// 匹配不上，卡片只能显示原始键名——本用例让这种漂移在 go test 就红。
func TestSnapshotObsJSONKeys(t *testing.T) {
	obs := SupplierSnapshotObs{Loaded: true, Revision: 7, LastSuccessUnixMs: 1_763_000_000_000, StaleAgeMs: 1_500}

	raw, err := json.Marshal(obs)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, map[string]any{
		"loaded":               true,
		"revision":             float64(7),
		"last_success_unix_ms": float64(1_763_000_000_000),
		"stale_age_ms":         float64(1_500),
	}, m, "snapshot 线上键必须是 snake_case 四键（不得出现 PascalCase 字段名）")
}

// TestViewLoadStatsSnapshotJSONKeys ops 条目（supplier-view）的嵌套形状：stats 顶层是
// attempts / success / snapshot，snapshot 内部必须是上面那四个小写键；omitempty 的
// nil 快照不出现（未注入观测面 = 不带该字段，而不是 `"snapshot":null`）。
func TestViewLoadStatsSnapshotJSONKeys(t *testing.T) {
	stats := ViewLoadStats{
		Attempts: 3,
		Success:  2,
		Snapshot: &SupplierSnapshotObs{Loaded: true, Revision: 7, LastSuccessUnixMs: 1, StaleAgeMs: 2},
	}
	raw, err := json.Marshal(stats)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Len(t, m, 3)
	require.Equal(t, float64(3), m["attempts"])
	require.Equal(t, float64(2), m["success"])
	require.Equal(t, map[string]any{
		"loaded":               true,
		"revision":             float64(7),
		"last_success_unix_ms": float64(1),
		"stale_age_ms":         float64(2),
	}, m["snapshot"], "snapshot 子键必须是 snake_case")

	empty, err := json.Marshal(ViewLoadStats{})
	require.NoError(t, err)
	var emptyMap map[string]any
	require.NoError(t, json.Unmarshal(empty, &emptyMap))
	require.NotContains(t, emptyMap, "snapshot", "nil 快照（未装配观测面）不出现该键")
}
