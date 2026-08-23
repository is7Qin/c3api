// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"time"
)

// 价格解析核：entry → ResolvedPrices 基底 + 首中即停变体应用。纯函数、
// 零外部依赖——service 快照读与测试假实现共用同一实现（防逻辑漂移）。

// ResolveEntryPrices 统一价格解析：基底取 entry 分量，按 seq 升序找第一条
// 匹配（tier/上下文/时段/星期 AND 组合）的变体应用 mult_bp/set_* 后返回。
// variants 无需预排序——调用方按 seq 升序传入；首中即停。
func ResolveEntryPrices(entry *PriceEntry, variants []*PriceVariant, tier string, promptTokens int64, at time.Time) (ResolvedPrices, bool) {
	if entry == nil {
		return ResolvedPrices{}, false
	}
	rp := ResolvedPrices{
		Mode:           entry.Mode,
		InputPerM:      entry.InputPerM,
		OutputPerM:     entry.OutputPerM,
		CacheReadPerM:  entry.CacheReadPerM,
		CacheWritePerM: entry.CacheWritePerM,
		PricePerCall:   entry.PricePerCall,
		ImgInTokPerM:   entry.ImgInTokPerM,
		ImgOutTokPerM:  entry.ImgOutTokPerM,
		PricePerImage:  entry.PricePerImage,
		Provider:       entry.Provider,
	}
	for _, v := range variants {
		if !variantMatches(v, tier, promptTokens, at) {
			continue
		}
		seq := v.Seq
		rp.VariantSeq = &seq
		if v.MultBP != nil {
			mult := int64(*v.MultBP)
			if mult < 0 {
				mult = 0
			} else if mult > 100000 {
				mult = 100000
			}
			applyMult := func(p **int64) {
				if *p != nil {
					val := (**p*mult + 5000) / 10000
					if val < 0 {
						return
					}
					// 复制新指针，禁止改写快照内分量（并发读者可见性）
					nv := val
					*p = &nv
				}
			}
			applyMult(&rp.InputPerM)
			applyMult(&rp.OutputPerM)
			applyMult(&rp.CacheReadPerM)
			applyMult(&rp.CacheWritePerM)
			applyMult(&rp.PricePerCall)
			applyMult(&rp.ImgInTokPerM)
			applyMult(&rp.ImgOutTokPerM)
			applyMult(&rp.PricePerImage)
		}
		if v.SetInputPerM != nil {
			nv := *v.SetInputPerM
			rp.InputPerM = &nv
		}
		if v.SetOutputPerM != nil {
			nv := *v.SetOutputPerM
			rp.OutputPerM = &nv
		}
		break
	}
	return rp, true
}

// variantMatches 变体条件匹配：全 nil = 通配；非 nil 条件全过才命中。
func variantMatches(v *PriceVariant, tier string, promptTokens int64, at time.Time) bool {
	if v.ServiceTier != nil && *v.ServiceTier != tier {
		return false
	}
	if v.CtxMin != nil && promptTokens < *v.CtxMin {
		return false
	}
	if v.CtxMax != nil && promptTokens >= *v.CtxMax {
		return false
	}
	if v.TimeStart != nil || v.TimeEnd != nil {
		if !timeMatches(v.TimeStart, v.TimeEnd, at) {
			return false
		}
	}
	if v.DowMask != nil {
		wd := int(at.Weekday()) // 0=Sun
		if (*v.DowMask>>wd)&1 == 0 {
			return false
		}
	}
	return true
}

func timeMatches(start, end *string, at time.Time) bool {
	if start == nil && end == nil {
		return true
	}
	// parse HH:MM
	parse := func(s string) int {
		var h, m int
		fmt.Sscanf(s, "%d:%d", &h, &m)
		return h*60 + m
	}
	cur := at.Hour()*60 + at.Minute()
	if start != nil && end != nil {
		s := parse(*start)
		e := parse(*end)
		if s == e {
			return true
		}
		if s < e {
			return cur >= s && cur < e
		}
		// midnight wrap
		return cur >= s || cur < e
	}
	if start != nil {
		s := parse(*start)
		return cur >= s
	}
	if end != nil {
		e := parse(*end)
		return cur < e
	}
	return true
}
