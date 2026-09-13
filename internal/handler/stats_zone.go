// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package handler

import (
	"time"

	"github.com/is7qin/c3api/internal/service"
)

// resolveStatsZone 统计端点 `timezone` 查询参数边界解析（request-browser-timezone-stats）：
// 缺省/空 → UTC（兼容）；非法 IANA 名 → service.ErrInvalidInput（调用方经
// httpface.WriteServiceErr 出 400）。返回已解析 *time.Location——未校验原始
// 串绝不出 handler 边界；结果只做请求内局部值，绝不落 AdminAPI/Service/Store
// 字段或包级全局。
func resolveStatsZone(raw *string) (*time.Location, error) {
	if raw == nil {
		return time.UTC, nil
	}
	return service.ResolveTimeZone(*raw)
}

// dayStart t 在 loc 时区的本地日历日零点（DST 安全：t.In(loc) + time.Date，
// 绝不用 UTC().Truncate(24h) 或固定 24h 算术）。返回绝对时刻。
func dayStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}
