// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestToAPIUsageLogCallComponent 按次分量投影（管理面 UsageLog）：CallCount /
// PricePerCallMillis 两列直通。价格 nil 必须保持 null——0 是"按次价配成 0 =
// 免费"，与"没有按次分量"语义不同，不能回落成 0。
func TestToAPIUsageLogCallComponent(t *testing.T) {
	price := int64(5400)
	dto := toAPIUsageLog(&domain.UsageLog{ID: 1, CallCount: 2, PricePerCallMillis: &price})
	require.NotNil(t, dto.CallCount)
	require.Equal(t, int64(2), *dto.CallCount)
	require.NotNil(t, dto.PricePerCallMillis)
	require.Equal(t, int64(5400), *dto.PricePerCallMillis)

	// PricePerCallMillis = nil → DTO 字段 nil（不是 0）
	dto = toAPIUsageLog(&domain.UsageLog{ID: 2, CallCount: 0, PricePerCallMillis: nil})
	require.NotNil(t, dto.CallCount)
	require.Equal(t, int64(0), *dto.CallCount)
	require.Nil(t, dto.PricePerCallMillis)

	// CallCount = 0 且价格非 nil → 两字段并存为 0（0 = 免费，不是缺失）
	zero := int64(0)
	dto = toAPIUsageLog(&domain.UsageLog{ID: 3, CallCount: 0, PricePerCallMillis: &zero})
	require.NotNil(t, dto.CallCount)
	require.Equal(t, int64(0), *dto.CallCount)
	require.NotNil(t, dto.PricePerCallMillis)
	require.Equal(t, int64(0), *dto.PricePerCallMillis)
}
