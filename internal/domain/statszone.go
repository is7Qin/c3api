// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// ZoneCubeReason 报告 loc 在绝对窗口 [from, to] 内能否由规范 UTC 小时卷积表
// （usage_stats/usage_entity_stats，桶界恒 UTC 整点）精确重组本地时区窗口，
// 并给出不能时的**唯一原因**（判序固定：先界 → 再偏移 → 再漂移）。
//
// 三条前提（旧布尔谓词的判序原样保留——它同时是「cube 能否表达
// 这次分组」这一问题的完整答案）：
//  1. 窗口两端恒 UTC 整点（秒/纳秒为零）。界不齐时 [from,to) 谓词会把边界上的
//     整小时卷积行劈成半行直接丢弃/纳入——cube 无法表达半个 UTC 小时，读取必须
//     改走原始行逐行聚合（repository stat_raw_read.go）。
//  2. 窗口内偏移恒为整小时（自 from 逐 UTC 小时采样，捕获 :30/:45 偏移）。
//  3. 窗口内偏移无跳变（捕获 DST/一次性偏移变更）。
//
// 成立的充要性：界齐整点 + 偏移恒整点 + 无跳变 ⟹ 任意本地小时/日界都落在 UTC
// 整点绝对时刻上、且不与窗口边界错半格 ⟹ 每条小时卷积行要么整体入窗要么整体
// 出窗，date_trunc(unit, t AT TIME ZONE loc) AT TIME ZONE loc 无歧义（偏移恒定
// → 墙钟↔绝对互逆双射）、无跨桶劈裂。反之（界不齐、DST 或 :30/:45 偏移）重组
// 会错桶/半行丢失/塌缩重复墙钟小时——读取必须改走原始 usage_logs/err_logs
// 按行精确聚合。
//
// nil loc 视同 UTC（仍受前提 1 约束）。采样步长 1h（窗口 ≤90d ⇒ ≤2161 次，
// 纯内存零 DB）。
type ZoneCubeReason uint8

const (
	// ZoneCubeReusable 三条前提全部成立：cube 可精确重组（无降级原因）。
	ZoneCubeReusable ZoneCubeReason = iota
	// ZoneCubeUnaligned 前提 1 失败：窗口界非 UTC 整点（界劈开卷积行）。
	ZoneCubeUnaligned
	// ZoneCubeOffset 前提 2 失败：窗口内偏移非整小时（:30/:45）。
	ZoneCubeOffset
	// ZoneCubeDST 前提 3 失败：窗口内偏移跳变（DST/一次性）。
	ZoneCubeDST
)

// ZoneCubeVerdict 判定 loc 在 [from, to] 上的 cube 可表达性。判序固定（先界 →
// 再偏移 → 再漂移），故返回的是**首个**失败原因，不是任意一个。
func ZoneCubeVerdict(loc *time.Location, from, to time.Time) ZoneCubeReason {
	if !hourAligned(from) || !hourAligned(to) {
		return ZoneCubeUnaligned // 窗口界劈开卷积行：raw 逐行才精确
	}
	if loc == nil || loc == time.UTC {
		return ZoneCubeReusable
	}
	baseOff := 0
	first := true
	for t := from; !t.After(to); t = t.Add(time.Hour) {
		_, off := t.In(loc).Zone()
		if off%3600 != 0 {
			return ZoneCubeOffset // 半小时/45 分偏移：小时桶界劈开卷积行
		}
		if first {
			baseOff, first = off, false
		} else if off != baseOff {
			return ZoneCubeDST // DST/一次性跳变：本地桶界在窗口内漂移
		}
	}
	return ZoneCubeReusable
}

// hourAligned t 恒为 UTC 整点：Unix 秒为 3600 整数倍且纳秒分量归零（分钟/秒/
// 亚秒任一分量非零即假整点）。
func hourAligned(t time.Time) bool {
	return t.Unix()%3600 == 0 && t.Nanosecond() == 0
}
