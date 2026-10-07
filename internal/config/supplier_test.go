// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSupplierConfigDefaults 默认值锚（spec 2026-10-09 §6.4）。
func TestSupplierConfigDefaults(t *testing.T) {
	d := defaults().Supplier
	require.False(t, d.Enabled, " supplier.enabled 默认 false（关闭态零成本）")
	require.True(t, d.FreezeEnabled, "supplier.freeze_enabled 默认 true")
	require.Equal(t, 24, d.FreezeHours)
	require.Equal(t, 2*time.Hour, d.ThawGranularity)
	require.Equal(t, 1000, d.ShareBpDefault)
	require.Equal(t, 10000, d.PayoutMaxBacklogRows)
	require.Equal(t, 10*time.Minute, d.PayoutMaxBacklogAge)
	require.Equal(t, 3*time.Minute, d.PayoutMaxObserveAge)
	require.Equal(t, 24*time.Hour, d.RiskReviewMaxAge)
	require.NoError(t, validateSupplier(&d), "默认值必须通过 fail-fast 校验（24h/2h ⇒ 13 ≤ 64）")
}

// TestSupplierValidateShareBp A16②：share_bp=0 合法、10001 非法。
func TestSupplierValidateShareBp(t *testing.T) {
	require.NoError(t, ValidateSupplierShareBp(0), "0 合法")
	require.NoError(t, ValidateSupplierShareBp(10000), "10000 合法")
	for _, bad := range []int{-1, 10001, 99999} {
		require.Error(t, ValidateSupplierShareBp(bad), "%d 非法", bad)
	}
}

// TestSupplierValidateThawGranularity A16③：thaw_granularity=1s 非法（<1m）、
// 500ms 非法、非整秒非法；整秒且 ≥1m 合法。
func TestSupplierValidateThawGranularity(t *testing.T) {
	c := defaults().Supplier
	c.ThawGranularity = time.Second
	require.Error(t, validateSupplier(&c), "1s < 1m 非法")
	c.ThawGranularity = 500 * time.Millisecond
	require.Error(t, validateSupplier(&c), "500ms 非法")
	c.ThawGranularity = 90*time.Second + 500*time.Millisecond
	require.Error(t, validateSupplier(&c), "非整秒非法")
	c.ThawGranularity = 2 * time.Hour
	require.NoError(t, validateSupplier(&c), "2h 合法")
	c.ThawGranularity = 1 * time.Minute
	c.FreezeHours = 0 // 1m 粒度下 24h 会破跨字段上界——此处只验粒度整秒性
	require.NoError(t, validateSupplier(&c), "1m 合法（整秒且 ≥1m）")
}

// TestSupplierValidateCrossField A16④：跨字段上界 127h/2h 拒绝、126h/2h 接受
// （含乘前溢出防护）；默认 24h/2h 接受。
func TestSupplierValidateCrossField(t *testing.T) {
	g := 2 * time.Hour
	require.NoError(t, ValidateSupplierFreezeHours(24, g), "24h/2h ⇒ ceil(12)+1=13 ≤ 64")
	require.NoError(t, ValidateSupplierFreezeHours(126, g), "126h/2h ⇒ ceil(63)+1=64 ≤ 64 接受")
	require.Error(t, ValidateSupplierFreezeHours(127, g), "127h/2h ⇒ ceil(63.5)+1=65 > 64 拒绝")
	require.Error(t, ValidateSupplierFreezeHours(-1, g), "负数拒绝")
	// 乘前溢出防护：极大 freeze_hours 不 panic、直接拒绝。
	require.Error(t, ValidateSupplierFreezeHours(int(^uint(0)>>1), g), "极大整数拒绝（不溢出）")
}

// TestSupplierValidatePayout A16⑧：付款风控四键边界。
func TestSupplierValidatePayout(t *testing.T) {
	base := defaults().Supplier
	base.PayoutMaxBacklogRows = 0
	require.NoError(t, validateSupplier(&base), "rows=0 合法下界")
	base.PayoutMaxBacklogRows = -1
	require.Error(t, validateSupplier(&base), "rows<0 拒绝")

	c := defaults().Supplier
	c.PayoutMaxBacklogAge = time.Second
	require.NoError(t, validateSupplier(&c), "backlog_age=1s 合法下界")
	c.PayoutMaxBacklogAge = 999 * time.Millisecond
	require.Error(t, validateSupplier(&c), "backlog_age<1s 拒绝")

	c = defaults().Supplier
	c.PayoutMaxObserveAge = time.Second
	require.NoError(t, validateSupplier(&c), "observe_age=1s 合法下界")
	c.PayoutMaxObserveAge = 0
	require.Error(t, validateSupplier(&c), "observe_age<1s 拒绝")

	c = defaults().Supplier
	c.RiskReviewMaxAge = time.Second
	require.NoError(t, validateSupplier(&c), "risk_review_max_age=1s 合法下界")
	c.RiskReviewMaxAge = 0
	require.Error(t, validateSupplier(&c), "risk_review_max_age<1s 拒绝")
}
