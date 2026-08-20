// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeOpsWorker 测试替身（StatsProvider 契约）。
type fakeOpsWorker struct {
	name  string
	stats any
}

func (f fakeOpsWorker) Name() string { return f.name }
func (f fakeOpsWorker) Stats() any   { return f.stats }

// typedStats 模拟各 worker 模块的 Stats 返回（typed struct，非 map）。
type typedStats struct {
	Pending int64 `json:"pending"`
}

// TestGetOpsWorkersResponse 响应 typed struct：workers 条目名称 + stats 透传
// （struct → JSON → map roundtrip）、snapshots 区、generated_at。路由走契约
// chi-server（BaseURL /admin → /api/admin/ops/workers）。
func TestGetOpsWorkersResponse(t *testing.T) {
	h := New(nil, OpsOptions{
		Workers: []StatsProvider{
			fakeOpsWorker{"billing", typedStats{Pending: 3}},
			fakeOpsWorker{"retention", typedStats{Pending: 0}},
		},
		Snapshots: func() []SnapshotState {
			return []SnapshotState{{Name: "auth", Scopes: &[]string{"settings"}, LastReload: time.Now().UTC()}}
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)

	var resp WorkersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Workers, 2)
	require.Equal(t, "billing", resp.Workers[0].Name)
	// stats 直出 typed struct 的 JSON 透传（解码后 map 形态，字段断言）。
	st0, ok := resp.Workers[0].Stats.(map[string]any)
	require.True(t, ok, "stats 应为对象")
	require.Equal(t, float64(3), st0["pending"])
	st1, ok := resp.Workers[1].Stats.(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(0), st1["pending"])
	require.Len(t, resp.Snapshots, 1)
	require.Equal(t, "auth", resp.Snapshots[0].Name)
	require.False(t, resp.GeneratedAt.IsZero(), "generated_at 非零")
}

// TestGetOpsWorkersNoSnapshots 未装配快照区 → snapshots 为 [] 非 null（契约
// required 字段，JSON 不得缺省）；零值 OpsOptions（未 WithOps）→ 空 workers。
func TestGetOpsWorkersNoSnapshots(t *testing.T) {
	h := New(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.JSONEq(t, `[]`, string(raw["snapshots"]), "未装配快照区 → [] 非 null")
	require.JSONEq(t, `[]`, string(raw["workers"]))

	var resp WorkersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Workers)
	require.Empty(t, resp.Snapshots)
}
