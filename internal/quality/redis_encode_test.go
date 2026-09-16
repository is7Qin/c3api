// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package quality

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func encodeTestKey(seed byte) Key {
	var k Key
	k.IdentityVersion = int16(seed)
	for i := range k.RouteClassID {
		k.RouteClassID[i] = seed + byte(i)
	}
	for i := range k.QualityClassID {
		k.QualityClassID[i] = 0xf0 - seed - byte(i)
	}
	for i := range k.Fingerprint {
		k.Fingerprint[i] = seed*3 + byte(i)*7
	}
	return k
}

func encodeTestMinute(k Key) *QualityMinute {
	qm := NewQualityMinute(1789000060, k)
	qm.attempts = 11
	qm.successes = 9
	qm.err429 = 1
	qm.err4xx = 2
	qm.err5xx = 3
	qm.errNetwork = 4
	qm.ttftCount = 7
	qm.sumQ32 = 123456789
	qm.sumSqQ32 = -987654321
	for i := range qm.hist {
		qm.hist[i] = int64(i+1) * 1000
	}
	qm.inputTokens = 123
	qm.outputTokens = 456
	qm.cacheRead = 789
	qm.cacheCreate = 321
	qm.calls = 5
	qm.images = 2
	return qm
}

// legacyQualityCellMap 冻结旧发布路径的 map 字面量（sync.go 历史版本）：
// parity 测试与基准的 oracle，防止"改完两边一起错"。
func legacyQualityCellMap(instanceSrc string, minute, seq int64, k Key, qm *QualityMinute) map[string]any {
	return map[string]any{
		"identity_version":  k.IdentityVersion,
		"route_class_id":    hex.EncodeToString(k.RouteClassID[:]),
		"quality_class_id":  hex.EncodeToString(k.QualityClassID[:]),
		"fingerprint":       hex.EncodeToString(k.Fingerprint[:]),
		"instance_src":      instanceSrc,
		"bucket_minute":     minute,
		"absolute_sequence": seq,
		"attempts":          qm.attempts,
		"successes":         qm.successes,
		"count_429":         qm.err429,
		"count_4xx":         qm.err4xx,
		"count_5xx":         qm.err5xx,
		"count_network":     qm.errNetwork,
		"ttft_n":            qm.ttftCount,
		"ttft_sum_q32":      qm.sumQ32,
		"ttft_sumsq_q32":    qm.sumSqQ32,
		"ttft_hist":         qm.hist[:],
		"input_tokens":      qm.inputTokens,
		"output_tokens":     qm.outputTokens,
		"cache_read":        qm.cacheRead,
		"cache_create":      qm.cacheCreate,
		"calls":             qm.calls,
		"images":            qm.images,
	}
}

// TestAppendQualityCellJSONMatchesLegacyMarshal 逐字段等价：手写编码 vs 旧
// map[string]any+json.Marshal 的 JSON 语义必须一致（对象键序无语义）。
func TestAppendQualityCellJSONMatchesLegacyMarshal(t *testing.T) {
	k := encodeTestKey(0x21)
	qm := encodeTestMinute(k)
	got := appendQualityCellJSON(nil, "host-4242-deadbeef", 1789000060, 7, k, qm)

	want, err := json.Marshal(legacyQualityCellMap("host-4242-deadbeef", 1789000060, 7, k, qm))
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
}

// TestAppendQualityFieldMatchesHexJoin field 三段小写 hex 用 ":" 拼接，
// 与旧 hex.EncodeToString 拼接逐字节一致。
func TestAppendQualityFieldMatchesHexJoin(t *testing.T) {
	k := encodeTestKey(0x07)
	require.Equal(t,
		hex.EncodeToString(k.RouteClassID[:])+":"+hex.EncodeToString(k.QualityClassID[:])+":"+hex.EncodeToString(k.Fingerprint[:]),
		string(appendQualityField(nil, k)))
}

// TestAppendQualityCellJSONReusedBuffer 复用缓冲的子切片安全性：发布路径把
// buf[field]/buf[val] 子切片交给 pipeline，append 越过旧 len（含底层数组扩容）
// 不得改写已捕获区间——先捕获、后校验首/中/末条内容。
func TestAppendQualityCellJSONReusedBuffer(t *testing.T) {
	var buf []byte
	var vals [][]byte
	for i := 0; i < 300; i++ {
		k := encodeTestKey(byte(i))
		start := len(buf)
		buf = appendQualityCellJSON(buf, "h", 42, int64(i), k, encodeTestMinute(k))
		vals = append(vals, buf[start:len(buf)])
	}
	for _, idx := range []int{0, 150, 299} {
		var m map[string]any
		require.NoError(t, json.Unmarshal(vals[idx], &m), "captured slice %d must stay intact", idx)
		require.Equal(t, float64(idx), m["absolute_sequence"], "captured slice %d must keep its own payload", idx)
	}
}

// BenchmarkQualityCellLegacyMarshal vs BenchmarkQualityCellAppendHTTPSet：
// 旧发布路径（map+json.Marshal+4×hex+逐 cell 字符串）vs 手写编码（调用方
// 缓冲 + 子切片引用）。证据用途：验证 P1 修复的 allocs/op 降幅。
func BenchmarkQualityCellLegacyMarshal(b *testing.B) {
	k := encodeTestKey(0x21)
	qm := encodeTestMinute(k)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		field := hex.EncodeToString(k.RouteClassID[:]) + ":" + hex.EncodeToString(k.QualityClassID[:]) + ":" + hex.EncodeToString(k.Fingerprint[:])
		val, _ := json.Marshal(legacyQualityCellMap("host-4242-deadbeef", 1789000060, int64(i), k, qm))
		sinkField = field
		sinkVal = string(val)
	}
}

func BenchmarkQualityCellAppend(b *testing.B) {
	k := encodeTestKey(0x21)
	qm := encodeTestMinute(k)
	buf := make([]byte, 0, 2048)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		fieldStart := len(buf)
		buf = appendQualityField(buf, k)
		valStart := len(buf)
		buf = appendQualityCellJSON(buf, "host-4242-deadbeef", 1789000060, int64(i), k, qm)
		sinkBytes = buf[fieldStart:valStart]
		sinkBytes = buf[valStart:]
	}
}

var (
	sinkField string
	sinkVal   string
	sinkBytes []byte
)
