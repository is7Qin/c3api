// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// TestToAPIAccountViewMapsHealth 钉住冷却明细的契约投影（spec 2026-11-14 §3.3）：
// 有冷却 ⇒ state/until/routes 全部落线；无记录 ⇒ health 字段整体缺席（omitempty），
// **不**编造 ready——「无记录」与「READY 记录存在」必须是两种线格式。
func TestToAPIAccountViewMapsHealth(t *testing.T) {
	until := time.Date(2026, 11, 14, 13, 30, 0, 0, time.UTC)
	out := toAPIAccountView(&service.AccountView{
		Account: &domain.Account{ID: 7, Name: "acc-7"},
		Health:  &service.AccountHealthView{State: "retry_after", Until: &until, Routes: 2},
	})
	require.NotNil(t, out.Health)
	require.Equal(t, "retry_after", out.Health.State)
	require.NotNil(t, out.Health.Until)
	require.True(t, until.Equal(*out.Health.Until))
	require.NotNil(t, out.Health.Routes)
	require.Equal(t, 2, *out.Health.Routes)

	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var wire struct {
		Health struct {
			State  string `json:"state"`
			Until  string `json:"until"`
			Routes int    `json:"routes"`
		} `json:"health"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, "retry_after", wire.Health.State)
	require.Equal(t, "2026-11-14T13:30:00Z", wire.Health.Until)
	require.Equal(t, 2, wire.Health.Routes)

	// 无 cooling 记录：字段缺席（omitempty），不是 {"state":null}。
	plain := toAPIAccountView(&service.AccountView{Account: &domain.Account{ID: 8, Name: "acc-8"}})
	require.Nil(t, plain.Health)
	raw, err = json.Marshal(plain)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"health"`)
}
