// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEvaluatePayoutGate 付款风控门放行谓词（spec 2026-10-09 §6.5）表驱动：
// 失败闭合——缺一项即拒绝。
func TestEvaluatePayoutGate(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	base := PayoutGateConfig{
		BillingEnabled: true,
		MaxBacklogRows: 10000,
		MaxBacklogAge:  10 * time.Minute,
		MaxObserveAge:  3 * time.Minute,
	}
	old := now.Add(-5 * time.Minute)
	tooOld := now.Add(-11 * time.Minute)
	recent := now.Add(-1 * time.Minute)
	fresh := now.Add(-30 * time.Second)
	future := now.Add(time.Minute)

	cases := []struct {
		name    string
		cfg     PayoutGateConfig
		probe   BillingProbe
		wantErr bool
	}{
		{"billing disabled", PayoutGateConfig{MaxBacklogRows: 1, MaxBacklogAge: time.Minute, MaxObserveAge: time.Minute}, BillingProbe{ObservedAt: fresh}, true},
		{"healthy empty queue", base, BillingProbe{BacklogRows: 0, ObservedAt: fresh}, false},
		{"backlog within rows and age", base, BillingProbe{BacklogRows: 5000, OldestCreatedAt: &recent, ObservedAt: fresh}, false},
		{"backlog over rows", base, BillingProbe{BacklogRows: 10001, OldestCreatedAt: &recent, ObservedAt: fresh}, true},
		{"backlog head too old", base, BillingProbe{BacklogRows: 10, OldestCreatedAt: &tooOld, ObservedAt: fresh}, true},
		{"backlog present but head missing", base, BillingProbe{BacklogRows: 10, ObservedAt: fresh}, true},
		{"observation stale", base, BillingProbe{BacklogRows: 0, ObservedAt: now.Add(-4 * time.Minute)}, true},
		{"observation in future", base, BillingProbe{BacklogRows: 0, ObservedAt: future}, true},
		{"negative backlog", base, BillingProbe{BacklogRows: -1, ObservedAt: fresh}, true},
		{"boundary rows exact within age", base, BillingProbe{BacklogRows: 10000, OldestCreatedAt: &old, ObservedAt: fresh}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EvaluatePayoutGate(tc.cfg, tc.probe, now)
			if tc.wantErr {
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrPayoutGate))
				return
			}
			require.NoError(t, err)
		})
	}
}
