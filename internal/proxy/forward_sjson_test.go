// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// setModel/setStreamAndModel/stripServiceTier sjson 字节级改写（E1）测试：
// 精度钉住（>2^53 整数无损）+ 字节保真（改写区外逐字节相同）+ 守卫短路
// 零分配 + 缺失路径补字段/删除不存在键语义 + 非法 JSON 错误面。
package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestSetModelPreservesBigInt 精度钉住：body 含 >2^53 整数，setModel 改写后
// 精确保留。旧 map 往返经 float64 静默损为 ...992；sjson 单字段 splice 保真。
func TestSetModelPreservesBigInt(t *testing.T) {
	body := []byte(`{"user":{"id":9007199254740993},"model":"gpt-4o","messages":[]}`)
	out, err := setModel(body, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "user.id").Raw)
	require.Contains(t, string(out), `"id":9007199254740993`)
	require.Equal(t, "gpt-5.6", gjson.GetBytes(out, "model").String())
}

// TestSetStreamAndModelPreservesBigInt 同上：双字段改写路径同样保真。
func TestSetStreamAndModelPreservesBigInt(t *testing.T) {
	body := []byte(`{"user":{"id":9007199254740993},"model":"gpt-4o","stream":true}`)
	out, err := setStreamAndModel(body, false, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "user.id").Raw)
	require.False(t, gjson.GetBytes(out, "stream").Bool())
	require.Equal(t, "gpt-5.6", gjson.GetBytes(out, "model").String())
}

// TestStripServiceTierPreservesBigInt strip 删除路径同样保真。
func TestStripServiceTierPreservesBigInt(t *testing.T) {
	body := []byte(`{"user":{"id":9007199254740993},"model":"gpt-4o","service_tier":"fast","stream":true}`)
	out, err := stripServiceTier(body)
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "user.id").Raw)
	require.Equal(t, `{"user":{"id":9007199254740993},"model":"gpt-4o","stream":true}`, string(out))
}

// TestSetModelByteFidelity 字节保真：仅目标字段值被替换，其余字节逐字节相同
// （键序不变——map 往返会键排序）。
func TestSetModelByteFidelity(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true,"model":"gpt-4o","temperature":1.5,"n":3}`)
	out, err := setModel(body, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"messages":[{"role":"user","content":"hi"}],"stream":true,"model":"gpt-5.6","temperature":1.5,"n":3}`, string(out))
}

// TestSetModelByteFidelityEscaped 字节保真：HTML 特殊字符区段原样保留（map
// 往返会转义 < > & 为 < 等）；ASCII 模型名 sjson 零转义。
func TestSetModelByteFidelityEscaped(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"a<b & c>d"}],"model":"gpt-4o","temperature":1.5}`)
	out, err := setModel(body, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"messages":[{"role":"user","content":"a<b & c>d"}],"model":"gpt-5.6","temperature":1.5}`, string(out))
}

// TestSetStreamAndModelByteFidelity 双字段改写：两处 splice，键序原样。
func TestSetStreamAndModelByteFidelity(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o","stream":true,"n":3}`)
	out, err := setStreamAndModel(body, false, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"messages":[{"role":"user","content":"hi"}],"model":"gpt-5.6","stream":false,"n":3}`, string(out))
}

// TestSetModelGuardZeroAlloc 守卫短路：model 已为目标值 → 返回原切片（零分配）。
func TestSetModelGuardZeroAlloc(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true}`)
	out, err := setModel(body, "gpt-4o")
	require.NoError(t, err)
	require.True(t, &out[0] == &body[0], "守卫命中必须返回原切片（零分配）")
}

// TestSetStreamAndModelGuardZeroAlloc 双守卫短路：stream/model 均已正确 →
// 返回原切片（零分配）。
func TestSetStreamAndModelGuardZeroAlloc(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true}`)
	out, err := setStreamAndModel(body, true, "gpt-4o")
	require.NoError(t, err)
	require.True(t, &out[0] == &body[0], "双守卫命中必须返回原切片（零分配）")
}

// TestSetStreamAndModelPartialGuard stream 已正确但 model 需改写：守卫未全中
// → 两字段都走 sjson 改写（与 map 版本恒重写两字段一致）。
func TestSetStreamAndModelPartialGuard(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true,"n":1}`)
	out, err := setStreamAndModel(body, true, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"model":"gpt-5.6","stream":true,"n":1}`, string(out))
}

// TestSetModelNullOrMissingRewritten null/缺失 model（守卫未命中）→ sjson
// 覆写/补字段，与 map 版本语义一致。
func TestSetModelNullOrMissingRewritten(t *testing.T) {
	out, err := setModel([]byte(`{"model":null,"stream":true}`), "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"model":"gpt-5.6","stream":true}`, string(out))

	out, err = setModel([]byte(`{"stream":true}`), "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"stream":true,"model":"gpt-5.6"}`, string(out), "缺失路径 sjson 默认加字段")
}

// TestSetStreamAndModelAppendsMissing stream/model 缺失 → sjson 补字段。
func TestSetStreamAndModelAppendsMissing(t *testing.T) {
	out, err := setStreamAndModel([]byte(`{"model":"gpt-4o"}`), true, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, `{"model":"gpt-5.6","stream":true}`, string(out))
}

// TestStripServiceTierMissingKey 字段缺失 → sjson 删除不存在键 = 无操作返回
// 原字节（与 map 版 delete 缺失键语义一致）。
func TestStripServiceTierMissingKey(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true}`)
	out, err := stripServiceTier(body)
	require.NoError(t, err)
	require.Equal(t, string(body), string(out))
}

// TestRewriteInvalidJSONGated 非法 JSON 错误面：caller.go 的 json.Valid 硬门
// 先于改写路径拒绝——setModel 等实际不可见非法 JSON（sjson 底层 gjson 宽容
// 解析，对 unquoted key 等畸形输入不报错、产出残缺字节；故错误面由门卫承担
// 而非函数自身，与 spec 错误面章节一致）。
func TestRewriteInvalidJSONGated(t *testing.T) {
	for _, body := range []string{`{not json`, `{"a":`, `{`, `{"a":1,`} {
		require.False(t, json.Valid([]byte(body)), "body=%s", body)
	}
}

// TestRewriteScalarRootErrors 顶层标量/数组根错误面（json.Valid 可通过、仅
// 恶意输入可达）：sjson 对顶层标量整体替换为 {}（非报错）与 map 版 Unmarshal
// 报错语义分歧——isJSONObjectRoot 守卫保持旧错误语义（旧版 null 体还会 nil
// map 赋值 panic）；顶层数组由 sjson 自行报错。
func TestRewriteScalarRootErrors(t *testing.T) {
	for _, body := range []string{`123`, `null`, `"str"`, `true`, `  `} {
		_, err := setModel([]byte(body), "gpt-5.6")
		require.Error(t, err, "body=%s", body)
		_, err = setStreamAndModel([]byte(body), true, "gpt-5.6")
		require.Error(t, err, "body=%s", body)
		_, err = stripServiceTier([]byte(body))
		require.Error(t, err, "body=%s", body)
	}
	_, err := setModel([]byte(`[1,2]`), "gpt-5.6")
	require.Error(t, err)
	_, err = setStreamAndModel([]byte(`[1,2]`), true, "gpt-5.6")
	require.Error(t, err)
	_, err = stripServiceTier([]byte(`[1,2]`))
	require.Error(t, err)
}
