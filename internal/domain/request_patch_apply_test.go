// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// rawJSON 是测试里构造 set 值的便利函数（值必须是合法 JSON 字面量）。
func rawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	require.True(t, json.Valid([]byte(s)), "测试夹具的 set 值必须是合法 JSON：%s", s)
	return json.RawMessage(s)
}

// TestApplyRequestBodyPatchNilAndEmpty 无配置 ⇒ 原 slice 原样返回（同一底层数组，
// 零拷贝；proxy 侧「无配置零成本」的执行器半边）。
func TestApplyRequestBodyPatchNilAndEmpty(t *testing.T) {
	body := []byte(`{"model":"gpt-4o"}`)
	got, err := ApplyRequestBodyPatch(body, nil)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.Same(t, &body[0], &got[0], "nil patch 必须原样返回同一个 slice（零拷贝）")

	got, err = ApplyRequestBodyPatch(body, &FormatPatch{})
	require.NoError(t, err)
	require.Same(t, &body[0], &got[0], "空 patch 同样零拷贝")
}

// TestApplyRequestBodyPatchDesignSkips 非 JSON / 顶层标量 / 空 body ⇒ 设计性跳过：
// 原样返回 + nil error（§2.5/§7.4：预期内，不是失败、不 WARN）。
func TestApplyRequestBodyPatchDesignSkips(t *testing.T) {
	patch := &FormatPatch{
		Set:    map[string]json.RawMessage{"model": rawJSON(t, `"patched"`)},
		Delete: []string{"metadata.client_field"},
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"nil body", nil},
		{"empty body", []byte{}},
		{"whitespace body", []byte(" \t\n ")},
		{"multipart", []byte("--boundary form-data name=model gpt-image-1 --boundary--")},
		{"binary", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}},
		{"json scalar string", []byte(`"just-a-string"`)},
		{"json scalar number", []byte(`123`)},
		{"json null", []byte(`null`)},
		{"malformed json that starts like object", []byte(`{"a":`)},
		{"object with trailing garbage", []byte(`{"a":1} trailing`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyRequestBodyPatch(tc.body, patch)
			require.NoError(t, err, "设计性跳过不得返回错误")
			require.Equal(t, tc.body, got)
		})
	}
}

// TestApplyRequestBodyPatchDeleteThenSet delete 先做（路径排序）→ set 后做：
// 同一 path 同时出现时 set 胜；排序保证不同 map 迭代序下的字节一致。
func TestApplyRequestBodyPatchDeleteThenSet(t *testing.T) {
	body := []byte(`{"metadata":{"client_field":"drop-me","keep":"yes"},"max_output_tokens":100}`)

	t.Run("delete 生效", func(t *testing.T) {
		patch := &FormatPatch{Delete: []string{"metadata.client_field"}}
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.JSONEq(t, `{"metadata":{"keep":"yes"},"max_output_tokens":100}`, string(got))
	})

	t.Run("同一 path 同时出现 delete 与 set ⇒ set 胜", func(t *testing.T) {
		patch := &FormatPatch{
			Set:    map[string]json.RawMessage{"metadata.client_field": rawJSON(t, `"set-wins"`)},
			Delete: []string{"metadata.client_field"},
		}
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.JSONEq(t, `{"metadata":{"client_field":"set-wins","keep":"yes"},"max_output_tokens":100}`, string(got))
	})

	t.Run("set 覆盖既有值", func(t *testing.T) {
		patch := &FormatPatch{Set: map[string]json.RawMessage{"max_output_tokens": rawJSON(t, `4096`)}}
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.JSONEq(t, `{"metadata":{"client_field":"drop-me","keep":"yes"},"max_output_tokens":4096}`, string(got))
	})
}

// TestApplyRequestBodyPatchRawLiteralsPreserved 大整数/嵌套对象保原字面量：
// SetRawBytes 不经过 interface{}/float64，1e18 与 >2^53 的整数逐字保留；
// body 中未被 patch 触及的大整数字节同样原样保留。
func TestApplyRequestBodyPatchRawLiteralsPreserved(t *testing.T) {
	body := []byte(`{"model":"m","n":9007199254740993}`)
	patch := &FormatPatch{
		Set: map[string]json.RawMessage{
			"big":    rawJSON(t, `9007199254740993`),
			"nested": rawJSON(t, `{"a":1,"b":[true,null,"x"]}`),
		},
	}
	got, err := ApplyRequestBodyPatch(body, patch)
	require.NoError(t, err)
	require.Contains(t, string(got), `"big":9007199254740993`, "set 的大整数必须逐字保留")
	require.Contains(t, string(got), `"n":9007199254740993`, "body 中既有大整数不得经 float64 改写")
	require.Contains(t, string(got), `"nested":{"a":1,"b":[true,null,"x"]}`, "嵌套值原字节拼接")
}

// TestApplyRequestBodyPatchNestedAndArrayPaths 路径语义 = sjson/gjson：嵌套键与
// 数组下标都能改，数组根 body 同样可 patch（顶层 object/array 二者都生效）。
func TestApplyRequestBodyPatchNestedAndArrayPaths(t *testing.T) {
	t.Run("数组元素", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"text"},{"type":"image"}]}`)
		patch := &FormatPatch{
			Set:    map[string]json.RawMessage{"input.0.type": rawJSON(t, `"image_url"`)},
			Delete: []string{"input.1"},
		}
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.JSONEq(t, `{"input":[{"type":"image_url"}]}`, string(got))
	})

	t.Run("-1 下标按 sjson 语义追加元素", func(t *testing.T) {
		body := []byte(`{"tools":[{"n":1},{"n":2}]}`)
		patch := &FormatPatch{Set: map[string]json.RawMessage{"tools.-1.n": rawJSON(t, `9`)}}
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.JSONEq(t, `{"tools":[{"n":1},{"n":2},{"n":9}]}`, string(got))
	})

	t.Run("数组根 body", func(t *testing.T) {
		body := []byte(`[{"a":1}]`)
		patch := &FormatPatch{Set: map[string]json.RawMessage{"0.b": rawJSON(t, `2`)}}
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.JSONEq(t, `[{"a":1,"b":2}]`, string(got))
	})
}

// TestApplyRequestBodyPatchDeterministic 同一配置在不同 map 迭代序下产出逐字节
// 相同的 body（delete/set 双路径排序；spec §2.5「排序保证确定性」）。
func TestApplyRequestBodyPatchDeterministic(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","metadata":{"a":1,"b":2},"input":"hi"}`)
	patch := &FormatPatch{
		Set: map[string]json.RawMessage{
			"z":          rawJSON(t, `1`),
			"a":          rawJSON(t, `2`),
			"metadata.c": rawJSON(t, `3`),
			"metadata.a": rawJSON(t, `4`),
		},
		Delete: []string{"metadata.b", "not_there", "metadata.b"},
	}
	first, err := ApplyRequestBodyPatch(body, patch)
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		got, err := ApplyRequestBodyPatch(body, patch)
		require.NoError(t, err)
		require.Equal(t, string(first), string(got), "第 %d 次执行字节漂移：排序失效", i)
	}
}

// TestApplyRequestBodyPatchResidualFailure 合法 JSON 但 sjson 执行失败（极端
// 组合/脏值）：原 body 全字节返回 + error（不返回半应用结果，由调用方裸透传）。
// 构造：数组下标 999999999 超过 sjson 的 maxpad(100000)，SetRawBytes 报错。
func TestApplyRequestBodyPatchResidualFailure(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	patch := &FormatPatch{Set: map[string]json.RawMessage{"x.999999999": rawJSON(t, `1`)}}
	got, err := ApplyRequestBodyPatch(body, patch)
	require.Error(t, err, "sjson 残余失败必须返回 error")
	require.Equal(t, body, got, "失败时原 body 全字节返回（不得半应用）")
	require.Same(t, &body[0], &got[0], "失败路径零拷贝返回原 slice")
}
