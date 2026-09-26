// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
)

// ResolveStatsWindow 统计端点请求边界的**窗口形态解析**（P3，spec §7.3/§7.3b）：
// 把线上三字段（from/to/window，三态都可选）解码成 domain.Admit 唯一接受的一对
// 绝对时刻。窗口**恰择一**：
//
//	(from != nil && to != nil && window == nil) -> 绝对窗口，原样直透
//	(from == nil && to == nil && window != nil) -> 相对窗口，服务端自持 now：
//	                                               to = ceilHour(now)、from = to − d
//	其余（两态都给 / 只给一端 / 两态都不给）    -> 400 reason=window_ambiguous
//	window 不是 time.ParseDuration 能解析的时长串 -> 400 reason=window_invalid
//
// **这是解码，不是判定**——本函数不问"这个窗口能不能被服务"（cost / coverage /
// zone 三条判定全部且只由 domain.Admit 回答，Admit 仍是唯一判定入口）。为什么
// 落在这里：
//
//  1. 它必须**唯一**。6 个启用 `window` 的端点分居两个包（handler 与
//     handler/user），httpface 是本仓库既有的"两面包共享的请求参数边界"
//     （包注释里的 ClampLimit 就是同一理由的先例）；写两份 = 两处可漂移。
//  2. 它必须返回 **400 线缆载体**。只有 httpface 能同时命名 domain（取
//     StatsRejectWindowAmbiguous）与 serviceerr（取承载类型）而不成环
//     （httpface 不能 import internal/service）。放在 domain 则每个调用点都要把
//     `*domain.StatsWindowError` 再包成 `*serviceerr.StatsWindowError`——那层包装
//     就是本 spec 明令禁止的垫片。
//  3. 它**不能**放进 Admit。wire 形态（三个可选字段）在 domain 之外：Admit 收到
//     的已经是 (from, to)。把可选性灌进 domain 会让 domain 认识 HTTP 形态。
//
// **只收时长，不收日历天**（spec §7.3b，评审 m3）：`d` 不是 time.ParseDuration
// 的单位，`7d` 的拒绝是 ParseDuration 的默认行为，此处只负责把它映射成 400 +
// 机读 reason（用户裁决：`24h` 在 DST 切换日 ≠ 一个日历天，而本 API 唯一的对齐
// 基准是固定 1h 网格，另立 AddDate 日历语义会引入第二套窗口长度语义）。
//
// now 由调用方注入（handler 既有的可注入时钟 h.now）——本函数不读墙钟，故 A11/A12
// 可确定性断言。返回值 error 恒为 nil 或 `*serviceerr.StatsWindowError`（经
// WriteServiceErr 出 400 + 机读字段）。
func ResolveStatsWindow(from, to *time.Time, window *string, now time.Time) (time.Time, time.Time, error) {
	if from != nil && to != nil && window == nil {
		return *from, *to, nil
	}
	if from == nil && to == nil && window != nil {
		d, err := time.ParseDuration(*window)
		if err != nil {
			// 语法裁决：只收时长（`7d` 走这里）；不可解析 → 参数非法，
			// 与"必填/倒序"同类（window_invalid 只发 reason，不带伪造的生效窗口）。
			return time.Time{}, time.Time{}, &serviceerr.StatsWindowError{
				StatsWindowError: domain.StatsWindowError{Reject: domain.StatsRejectWindowInvalid},
			}
		}
		// 双界整点（domain.WindowFromDuration 用与 Admit 同一个 ceilHour），
		// 故按定义命中 Admit 第 1 条（精确）：零对齐位移、零桶丢失。
		f, t := domain.WindowFromDuration(d, now)
		return f, t, nil
	}
	return time.Time{}, time.Time{}, &serviceerr.StatsWindowError{
		StatsWindowError: domain.StatsWindowError{Reject: domain.StatsRejectWindowAmbiguous},
	}
}
