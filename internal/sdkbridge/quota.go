// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// 冷却截止的判定常量（设计 §3 数据卫生）。
const (
	// quotaHorizon 绝对上界（与窗口长度无关）：没有任何已知窗口长过这个尺度，
	// 于是 reset 远在它之外就不是"本窗口的重置点"，而是上游的垃圾/串号数值——
	// 照单全收会把账号钉死几个月（`UnixMilli` 甚至会溢出成负数）。
	// 它**必须独立于 `Limit` 求值**：`Limit` 是上游给的数，一条被写坏的巨大值
	// 就能把那条自校整条作废（见 ExhaustedUntil 第 4 条）。
	quotaHorizon = 31 * 24 * time.Hour
	// quotaResetSkew 绝对时刻（`ResetAt`）与相对秒数（`ResetAfter`）的互校容差。
	// 实测两者对同一窗口只差 1s（采样时刻 vs 上游取整），10s 留够抖动余量又足以
	// 拦住"绝对时刻被改写但相对值没变"的漂移。
	quotaResetSkew = 10 * time.Second
)

// ExhaustedUntil 从窗口事实里挑出「最晚的、可信的、仍然有效的 reset」。
//
// 它是纯函数（无 IO、无全局状态、不读时钟——`now` 由调用方给），也是"额度打满
// ⇒ 冷却到 reset"的**唯一**判决入口：本包不解析 `UsageStatus.Raw`、不看
// `RateLimitReachedType`、不碰 `model_usage`/`code_review_rate_limit`——那些要么
// 不是窗口事实，要么只存在于原始 JSON 里（设计 §2.2 第 1 行）。
//
// 卫生条件按顺序求值（顺序即语义：前一条不合格的窗口不再进入后续判断），任何
// 一条不过的窗口都只是**不参与**，不影响其他窗口：
//
//  1. !w.Reached                               → 不参与（没打满就不是冷却依据）；
//  2. w.ResetAt 为零值                          → 不参与（上游没给绝对时刻）；
//  3. !w.ResetAt.After(now)                     → 不参与（已过期或正好到点：窗口
//     已经/即将翻转，再冷却就是无限续期）；
//  4. 剩余 > quotaHorizon                       → 不参与（绝对上界，**与 `w.Limit`
//     无关**：没有任何已知窗口长过 31d，超过它就不是这个窗口的重置点。这条必须
//     独立求值，否则一条被写坏的大 `Limit` 就能把 5) 整条作废）；
//  5. w.Limit > 0 且 剩余 > 1.25×w.Limit        → 不参与（时刻与窗口长度不匹配）；
//     w.Limit == 0                              → 跳过本条，4) 的绝对上界已在兜底；
//  6. w.ResetAfter > 0 且 |剩余 - w.ResetAfter| > quotaResetSkew
//     → 不参与（绝对/相对互校失败）；
//     w.ResetAfter <= 0                         → 跳过本条，其余校验照做
//     （上游没给相对值 ≠ 时刻不可信）；
//  7. until = max(参与集合.ResetAt)；空集 → (zero, false)。
//
// 返回 false 表示"没有可信的截止时刻"，调用方据此保持原有冷却时长——它**不是**
// 错误：窗口都没打满、或事实全被剔除，都是正常结论。
func ExhaustedUntil(windows []codexsdk.QuotaWindow, now time.Time) (time.Time, bool) {
	var (
		until time.Time
		ok    bool
	)
	for _, w := range windows {
		// 1) 未打满：与 3) 无关——打满与否只看 used_percent，SDK 已机械派生。
		if !w.Reached {
			continue
		}
		// 2) 绝对时刻缺失（`reset_at` 未给/非正）：相对秒数再好看也无法变成时刻。
		if w.ResetAt.IsZero() {
			continue
		}
		// 3) 已过期/正好到点：`After` 而非 `!Before`，正好等于 now 也剔除——
		//    否则一个翻转瞬间的窗口能把冷却延长到"现在"，等于拿旧事实续命。
		if !w.ResetAt.After(now) {
			continue
		}
		// remaining 恒 > 0（3) 已保证）。
		remaining := w.ResetAt.Sub(now)
		// 4) 绝对上界（与 Limit 无关）：没有任何已知窗口长过 quotaHorizon，
		//    超过它的 reset 一律不是"本窗口的重置点"；这条必须独立于 Limit 求值，
		//    否则一条被写坏的 Limit（上游给的垃圾大数）就能把 5) 整条作废。
		//    顺带挡住 `ResetAt.Sub(now)` 的溢出——年尺度的剩余会被 `Sub` 夹成
		//    time.Duration 上限，照单全收会让 `UnixMilli` 溢出成负数。
		if remaining > quotaHorizon {
			continue
		}
		// 5) 与窗口长度自校。
		if w.Limit > 0 {
			// 写成减法而不是 `remaining > w.Limit*5/4`：Limit 可到 time.Duration
			// 的量级上限，乘 5 会溢出成负数（那会让任何剩余都"通过"）。
			if remaining > w.Limit && remaining-w.Limit > w.Limit/4 {
				continue
			}
		}
		// 6) 绝对/相对互校。`ResetAfter <= 0` = 上游没给相对值，跳过本条。
		if w.ResetAfter > 0 {
			skew := remaining - w.ResetAfter
			if skew < 0 {
				skew = -skew
			}
			if skew > quotaResetSkew {
				continue
			}
		}
		// 7) 取最晚（`time.Time` 不是有序类型，内建 max 用不了）。
		if !ok || w.ResetAt.After(until) {
			until, ok = w.ResetAt, true
		}
	}
	if !ok {
		return time.Time{}, false
	}
	return until, true
}
