// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package quality

import "strconv"

// appendQualityCellJSON 把一条质量 cell 编码为 Redis 载荷（23 个固定字段）。
//
// 第一性原理：该载荷字段集合编译期固定（无 schema 漂移面），逐 cell 走
// map[string]any + json.Marshal 会为每字段付出反射遍历 + interface 装箱 +
// map 键排序 + 中间 []byte 增长；发布频率（每 flush ≤10k cells）下它是全
// 进程第一大分配源（Mac 压测 heap alloc_space 实测：json 编码族 cum
// 12.20%、hex.EncodeToString 2.28%、含其上的 doRedisLocked cum 23.76%）。
// 手写编码把它压成"往调用方缓冲追加字节"：零 map、零装箱、零中间字符串。
// 字段值语义与旧 JSON 逐字段一致（对象键序无语义，消费端只做 JSON 解析；
// 唯一差异是旧 json.Marshal 会 HTML 转义 <>&，本编码不转义——instance_src
// 为 host-pid-hex，不含这些字符）。
func appendQualityCellJSON(dst []byte, instanceSrc string, minute, seq int64, k Key, qm *QualityMinute) []byte {
	dst = append(dst, `{"identity_version":`...)
	dst = strconv.AppendInt(dst, int64(k.IdentityVersion), 10)
	dst = append(dst, `,"route_class_id":"`...)
	dst = appendLowerHex(dst, k.RouteClassID[:])
	dst = append(dst, `","quality_class_id":"`...)
	dst = appendLowerHex(dst, k.QualityClassID[:])
	dst = append(dst, `","fingerprint":"`...)
	dst = appendLowerHex(dst, k.Fingerprint[:])
	dst = append(dst, `","instance_src":`...)
	dst = strconv.AppendQuote(dst, instanceSrc)
	dst = append(dst, `,"bucket_minute":`...)
	dst = strconv.AppendInt(dst, minute, 10)
	dst = append(dst, `,"absolute_sequence":`...)
	dst = strconv.AppendInt(dst, seq, 10)
	dst = append(dst, `,"attempts":`...)
	dst = strconv.AppendInt(dst, qm.attempts, 10)
	dst = append(dst, `,"successes":`...)
	dst = strconv.AppendInt(dst, qm.successes, 10)
	dst = append(dst, `,"count_429":`...)
	dst = strconv.AppendInt(dst, qm.err429, 10)
	dst = append(dst, `,"count_4xx":`...)
	dst = strconv.AppendInt(dst, qm.err4xx, 10)
	dst = append(dst, `,"count_5xx":`...)
	dst = strconv.AppendInt(dst, qm.err5xx, 10)
	dst = append(dst, `,"count_network":`...)
	dst = strconv.AppendInt(dst, qm.errNetwork, 10)
	dst = append(dst, `,"ttft_n":`...)
	dst = strconv.AppendInt(dst, qm.ttftCount, 10)
	dst = append(dst, `,"ttft_sum_q32":`...)
	dst = strconv.AppendInt(dst, qm.sumQ32, 10)
	dst = append(dst, `,"ttft_sumsq_q32":`...)
	dst = strconv.AppendInt(dst, qm.sumSqQ32, 10)
	dst = append(dst, `,"ttft_hist":[`...)
	for i, h := range qm.hist {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendInt(dst, h, 10)
	}
	dst = append(dst, `],"input_tokens":`...)
	dst = strconv.AppendInt(dst, qm.inputTokens, 10)
	dst = append(dst, `,"output_tokens":`...)
	dst = strconv.AppendInt(dst, qm.outputTokens, 10)
	dst = append(dst, `,"cache_read":`...)
	dst = strconv.AppendInt(dst, qm.cacheRead, 10)
	dst = append(dst, `,"cache_create":`...)
	dst = strconv.AppendInt(dst, qm.cacheCreate, 10)
	dst = append(dst, `,"calls":`...)
	dst = strconv.AppendInt(dst, qm.calls, 10)
	dst = append(dst, `,"images":`...)
	dst = strconv.AppendInt(dst, qm.images, 10)
	return append(dst, '}')
}

// appendQualityField 追加 HSet field（route:quality:fingerprint 三段小写
// hex，与旧 hex.EncodeToString 拼接逐字节一致）。
func appendQualityField(dst []byte, k Key) []byte {
	dst = appendLowerHex(dst, k.RouteClassID[:])
	dst = append(dst, ':')
	dst = appendLowerHex(dst, k.QualityClassID[:])
	dst = append(dst, ':')
	return appendLowerHex(dst, k.Fingerprint[:])
}

const lowerHexDigits = "0123456789abcdef"

func appendLowerHex(dst, src []byte) []byte {
	for _, b := range src {
		dst = append(dst, lowerHexDigits[b>>4], lowerHexDigits[b&0x0f])
	}
	return dst
}
