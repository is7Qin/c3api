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

// TestAppendQualityCellJSONGolden 冻结编码字节（下游按 JSON 解析；金字节防格式
// 漂移）。若需改格式，必须同步更新 golden 并在提交信息里给出理由。
func TestAppendQualityCellJSONGolden(t *testing.T) {
	k := encodeTestKey(0x21)
	qm := encodeTestMinute(k)
	const want = `{"identity_version":33,"route_class_id":"2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40","quality_class_id":"cfcecdcccbcac9c8c7c6c5c4c3c2c1c0bfbebdbcbbbab9b8b7b6b5b4b3b2b1b0","fingerprint":"636a71787f868d949ba2a9b0b7bec5ccd3dae1e8eff6fd040b121920272e353c","instance_src":"host-4242-deadbeef","bucket_minute":1789000060,"absolute_sequence":7,"attempts":11,"successes":9,"count_429":1,"count_4xx":2,"count_5xx":3,"count_network":4,"ttft_n":7,"ttft_sum_q32":123456789,"ttft_sumsq_q32":-987654321,"ttft_hist":[1000,2000,3000,4000,5000,6000,7000,8000,9000,10000],"input_tokens":123,"output_tokens":456,"cache_read":789,"cache_create":321,"calls":5,"images":2}`
	require.Equal(t, want, string(appendQualityCellJSON(nil, "host-4242-deadbeef", 1789000060, 7, k, qm)))
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

var sinkBytes []byte
