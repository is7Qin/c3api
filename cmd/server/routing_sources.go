// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package main

import (
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/scheduler"
)

// q32Scale TTFT 对数和定点缩放（与 quality recorder 写侧 / service frontier 同量）。
const q32Scale = float64(int64(1) << 32)

// compilerQualitySource 把 quality lane（recorder 活体 cell）接成 routing
// compiler 的质量输入源（Task18 装配，SetCompilerSources 的 qualityFn）：
// compiler 键 (RouteClassID, CandidateFingerprint) 不带 QualityClassID，同键
// 多质量类 cell 聚合计数；Q32 定点和换算 float；计数经 scheduler.Counts.Add
// 校验（非法/溢出贡献丢弃、保留已聚合并——fail-closed，坏观测不进决策）。
// 编译道专用后台读（recorder 互斥冷路径），请求路径零依赖。
// ponytail: live cell 为进程存活期累计（本实例实时面）；rollup 驱动道落地后
// 换 PG rollup 窗口 provider（跨实例合并），聚合口径不变。
func compilerQualitySource(rec *quality.Recorder) func() map[scheduler.CandidateQualityKey]scheduler.CandidateQualityInput {
	return func() map[scheduler.CandidateQualityKey]scheduler.CandidateQualityInput {
		cells := rec.LiveCells()
		if len(cells) == 0 {
			return nil
		}
		out := make(map[scheduler.CandidateQualityKey]scheduler.CandidateQualityInput, len(cells))
		for k, qm := range cells {
			ck := scheduler.CandidateQualityKey{
				RouteClassID: domain.RouteClassIDVal(k.RouteClassID),
				Fingerprint:  domain.CandidateFingerprintVal(k.Fingerprint),
			}
			cnt := scheduler.Counts{
				Attempts:  int(qm.Attempts()),
				Successes: int(qm.Successes()),
				SumLog:    float64(qm.SumQ32()) / q32Scale,
				SumSq:     float64(qm.SumSqQ32()) / q32Scale,
				TTFTCount: int(qm.TTFTCount()),
			}
			in := scheduler.CandidateQualityInput{
				Counts:            cnt,
				InputTokens:       qm.InputTokens(),
				OutputTokens:      qm.OutputTokens(),
				CacheReadTokens:   qm.CacheReadTokens(),
				CacheCreateTokens: qm.CacheCreateTokens(),
			}
			prev, seen := out[ck]
			if !seen {
				// 首入也走 Add 校验（zero.Add 强制 validCounts）。
				base, err := scheduler.Counts{}.Add(cnt)
				if err != nil {
					continue
				}
				in.Counts = base
				out[ck] = in
				continue
			}
			merged, err := prev.Counts.Add(cnt)
			if err != nil {
				continue
			}
			prev.Counts = merged
			prev.InputTokens += in.InputTokens
			prev.OutputTokens += in.OutputTokens
			prev.CacheReadTokens += in.CacheReadTokens
			prev.CacheCreateTokens += in.CacheCreateTokens
			out[ck] = prev
		}
		return out
	}
}
