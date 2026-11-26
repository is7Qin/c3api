// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// request_patch 的领域单测（spec custom-request-patch §2.6/§5.1/§5.2 的保存面）：
// 解析/归一/三态/错误矩阵、CanonicalBytes 的确定性与大整数原字节、path 文法。

// TestRequestPatchParseNormalizeAndCanonical 覆盖 spec §2.2 的完整示例形状：
// 头名规范形归一、{} 层删除、CanonicalBytes 的字段序/排序/空层省略，以及
// 「parse ∘ canonical 幂等」（规范形再解析后逐字节不变）。
func TestRequestPatchParseNormalizeAndCanonical(t *testing.T) {
	raw := `{
	  "headers": {"user-agent": "vibe-code/1.0", "x-call-source": "vibe_code", "x-drop": ""},
	  "formats": {
	    "openai-responses": {
	      "headers": {"openai-beta": "responses=v2"},
	      "set": {"max_output_tokens": 4096, "metadata.call_source": "vibe_code"},
	      "delete": ["metadata.client_field"]
	    },
	    "anthropic": {"set": {"metadata.user_id": "vibe_code"}},
	    "openai-chat": {}
	  }
	}`
	p, err := ParseRequestPatch(json.RawMessage(raw))
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, map[string]string{
		"User-Agent":    "vibe-code/1.0",
		"X-Call-Source": "vibe_code",
		"X-Drop":        "",
	}, p.Headers, "头名必须存规范形；空值 = Del 是有效条目（不是空层）")
	require.Len(t, p.Formats, 2, "空 format patch 层必须被删除")
	require.Contains(t, p.Formats, UpstreamPathKey("openai-responses"))
	require.Contains(t, p.Formats, UpstreamPathKey("anthropic"))

	respPatch := p.Formats[UpstreamPathKey("openai-responses")]
	// http.CanonicalHeaderKey 只把首字母与 '-' 后首字母大写（不是缩写词典）：
	// "openai-beta" ⇒ "Openai-Beta"；保存期存储的就是这个规范形。
	require.Equal(t, map[string]string{"Openai-Beta": "responses=v2"}, respPatch.Headers)
	require.Equal(t, json.RawMessage("4096"), respPatch.Set["max_output_tokens"])
	require.Equal(t, json.RawMessage(`"vibe_code"`), respPatch.Set["metadata.call_source"])
	require.Equal(t, []string{"metadata.client_field"}, respPatch.Delete)
	require.False(t, p.IsEmpty())

	// 规范字节：顶层 headers → formats；map 键排序；内层 headers → set → delete；
	// 空层省略。逐字钉住序列化格式，防止「看起来稳定」的实现漂移。
	want := `{"headers":{"User-Agent":"vibe-code/1.0","X-Call-Source":"vibe_code","X-Drop":""},"formats":{"anthropic":{"set":{"metadata.user_id":"vibe_code"}},"openai-responses":{"headers":{"Openai-Beta":"responses=v2"},"set":{"max_output_tokens":4096,"metadata.call_source":"vibe_code"},"delete":["metadata.client_field"]}}}`
	canonical := p.CanonicalBytes()
	require.Equal(t, want, string(canonical))

	// 幂等：规范形必须能被重新解析，且再序列化逐字节相同（P2 digest/落库前提）。
	again, err := ParseRequestPatch(canonical)
	require.NoError(t, err)
	require.Equal(t, string(canonical), string(again.CanonicalBytes()))
}

// TestRequestPatchCanonicalBytesDeterministic 同输入不同 map 迭代序 ⇒ 同字节；
// delete 排序；空 patch ⇒ nil。
func TestRequestPatchCanonicalBytesDeterministic(t *testing.T) {
	build := func(reverse bool) *RequestPatch {
		p := &RequestPatch{
			Headers: map[string]string{"X-A": "1", "X-B": "2", "X-C": ""},
			Formats: map[UpstreamPathKey]FormatPatch{
				UpstreamPathChat: {
					Headers: map[string]string{"Z": "z", "A": "a"},
					Set:     map[string]json.RawMessage{"b": json.RawMessage("1"), "a": json.RawMessage(`{"k":[1,2]}`)},
					Delete:  []string{"z.path", "a.path"},
				},
				UpstreamPathAnthropic: {Set: map[string]json.RawMessage{"m": json.RawMessage("null")}},
			},
		}
		if reverse {
			// 重建一份（同内容），意图是让 map 的迭代序在两次之间不可控地不同：
			// encoding/json 对 map 键排序，输出必须一致。
			p2, err := ParseRequestPatch(p.CanonicalBytes())
			require.NoError(t, err)
			return p2
		}
		return p
	}
	first := build(false).CanonicalBytes()
	second := build(true).CanonicalBytes()
	require.NotEmpty(t, first)
	require.Equal(t, string(first), string(second))
	require.Contains(t, string(first), `"delete":["a.path","z.path"]`, "delete 必须排序")

	var nilPatch *RequestPatch
	require.Nil(t, nilPatch.CanonicalBytes())
	empty, err := ParseRequestPatch(json.RawMessage(`{"headers":{},"formats":{}}`))
	require.NoError(t, err)
	require.Nil(t, empty)
	require.Nil(t, empty.CanonicalBytes())
}

// TestRequestPatchDeleteDedup delete 列表解析期去重（spec §17.4 / 评审 momus m3）：
// 重复项不产生不同配置，且与「本来就无重复」的配置规范字节完全相同（P2 digest
// 不因书写重复换 key）；首次出现顺序保留，CanonicalBytes 仍统一排序。
func TestRequestPatchDeleteDedup(t *testing.T) {
	dup, err := ParseRequestPatch(json.RawMessage(
		`{"formats":{"anthropic":{"delete":["b.c","a.b","b.c","a.b"]}}}`))
	require.NoError(t, err)
	require.Equal(t, []string{"b.c", "a.b"}, dup.Formats[UpstreamPathAnthropic].Delete,
		"重复 delete 项必须去重（保留首次出现顺序）")

	uniq, err := ParseRequestPatch(json.RawMessage(
		`{"formats":{"anthropic":{"delete":["a.b","b.c"]}}}`))
	require.NoError(t, err)
	require.Equal(t, string(uniq.CanonicalBytes()), string(dup.CanonicalBytes()),
		"去重后与无重复配置规范字节相同（digest 不因书写重复换 key）")

	// 重复项不是空层：其他字段为空时该层仍应保留（去重不得顺手删层）。
	single, err := ParseRequestPatch(json.RawMessage(
		`{"formats":{"anthropic":{"delete":["a.b","a.b"]}}}`))
	require.NoError(t, err)
	require.Equal(t, []string{"a.b"}, single.Formats[UpstreamPathAnthropic].Delete)
	require.NotNil(t, single)
}

// TestRequestPatchCanonicalBytesPreservesBigNumbers 大整数/1e18 不经过 float64：
// set 值原字节逐字保留（spec §5.1「RawMessage 保 JSON 值原样」、§11.11）。
func TestRequestPatchCanonicalBytesPreservesBigNumbers(t *testing.T) {
	raw := `{"formats":{"anthropic":{"set":{
	  "big": 9007199254740993,
	  "e18": 1e18,
	  "neg": -9223372036854775808,
	  "nested": {"n": 9007199254740993, "arr": [1.50, 2]},
	  "str": "9007199254740993"
	}}}}`
	p, err := ParseRequestPatch(json.RawMessage(raw))
	require.NoError(t, err)
	canonical := string(p.CanonicalBytes())
	for _, want := range []string{
		`"big":9007199254740993`,
		`"e18":1e18`, // 字面量保留：1e18 不得被改写成 1000000000000000000
		`"neg":-9223372036854775808`,
		// 嵌套对象在解析期做**确定性归一**：键按字典序、去空白（语义不变，digest/
		// 落库不受书写形态影响）；数组顺序保留。
		`"nested":{"arr":[1.50,2],"n":9007199254740993}`,
		`"str":"9007199254740993"`,
	} {
		require.Contains(t, canonical, want)
	}
	require.NotContains(t, canonical, "9007199254740992")

	// 深归一：同一嵌套对象的不同书写形态（键序/空白/HTML 字符）⇒ 同一规范字节。
	a, err := ParseRequestPatch(json.RawMessage(`{"formats":{"anthropic":{"set":{"o":{"z":1,"a":{"y":2,"b":3}},"s":"a&b<c>","arr":[3,1]}}}}`))
	require.NoError(t, err)
	b, err := ParseRequestPatch(json.RawMessage(`{"formats":{"anthropic":{"set":{"o":{"a":{"b":3,"y":2},"z":1},"s":"a&b<c>","arr":[3,1]}}}}`))
	require.NoError(t, err)
	require.Equal(t, string(a.CanonicalBytes()), string(b.CanonicalBytes()))
	require.Contains(t, string(a.CanonicalBytes()), `"o":{"a":{"b":3,"y":2},"z":1}`)
	require.Contains(t, string(a.CanonicalBytes()), `"s":"a&b<c>"`, `字符串里的 & < > 不得被 HTML 转义成 \u00XX`)
	require.Contains(t, string(a.CanonicalBytes()), `"arr":[3,1]`, "数组顺序是 JSON 语义，不得排序")
}

// TestRequestPatchHeaderCanonicalization 同层归一后同名 ⇒ 400 语义；跨层同名
// 合法（格式层覆盖全局层，spec §2.4/§2.6）。
func TestRequestPatchHeaderCanonicalization(t *testing.T) {
	_, err := ParseRequestPatch(json.RawMessage(`{"headers":{"X-A":"1","x-a":"2"}}`))
	require.Error(t, err, "同层归一后同名是歧义配置")

	_, err = ParseRequestPatch(json.RawMessage(`{"formats":{"anthropic":{"headers":{"X-A":"1","x-a":"2"}}}}`))
	require.Error(t, err, "格式层内同层同名同样歧义")

	p, err := ParseRequestPatch(json.RawMessage(`{"headers":{"x-a":"global"},"formats":{"anthropic":{"headers":{"X-A":"format"}}}}`))
	require.NoError(t, err, "跨层同名合法")
	require.Equal(t, "global", p.Headers["X-A"])
	require.Equal(t, "format", p.Formats[UpstreamPathAnthropic].Headers["X-A"])

	// 小写 host 必须归一（Host 覆盖是 spec I7 的既有语义；小写不归一会静默失效）。
	p, err = ParseRequestPatch(json.RawMessage(`{"headers":{"host":"example.com"}}`))
	require.NoError(t, err)
	require.Equal(t, "example.com", p.Headers["Host"])
}

// TestRequestPatchRejectsAcceptEncoding Accept-Encoding（规范形）非空值 ⇒ 保存期
// 硬拒（响应编码不变量，spec §2.4/§11.10）：大小写变体、全局层、格式层全部覆盖；
// 空串（Del）与其它头放行。
func TestRequestPatchRejectsAcceptEncoding(t *testing.T) {
	for _, raw := range []string{
		`{"headers":{"Accept-Encoding":"gzip"}}`,
		`{"headers":{"accept-encoding":"gzip"}}`,
		`{"headers":{"ACCEPT-ENCODING":"br"}}`,
		`{"formats":{"anthropic":{"headers":{"accept-encoding":"gzip"}}}}`,
	} {
		_, err := ParseRequestPatch(json.RawMessage(raw))
		require.Error(t, err, raw)
	}
	p, err := ParseRequestPatch(json.RawMessage(`{"headers":{"Accept-Encoding":""}}`))
	require.NoError(t, err, "空串 = Del，必须放行（Go 会追加默认 gzip 并透明解压）")
	require.Equal(t, "", p.Headers["Accept-Encoding"])

	p, err = ParseRequestPatch(json.RawMessage(`{"headers":{"X-Accept-Encoding":"gzip"}}`))
	require.NoError(t, err, "只拒精确的 Accept-Encoding，不误伤前缀相似的自定义头")
	require.Equal(t, "gzip", p.Headers["X-Accept-Encoding"])
}

// TestRequestPatchRejectsUnknownFormat 未知格式键 ⇒ error（防拼写错误静默失效，
// spec §2.3）；七键全部合法。
func TestRequestPatchRejectsUnknownFormat(t *testing.T) {
	_, err := ParseRequestPatch(json.RawMessage(`{"formats":{"openai-chatx":{"set":{"a":1}}}}`))
	require.Error(t, err)
	_, err = ParseRequestPatch(json.RawMessage(`{"formats":{"openai-chat":null,"unknown":null}}`))
	require.Error(t, err, "未知键即使层为 null 也要拒（否则拼写错误静默消失）")

	for _, key := range []string{
		"openai-chat", "openai-responses", "openai-responses-ws", "anthropic",
		"openai-search", "openai-images.generations", "openai-images.edits",
	} {
		_, err := ParseRequestPatch(json.RawMessage(`{"formats":{"` + key + `":{"set":{"a":1}}}}`))
		require.NoError(t, err, key)
	}
}

// TestRequestPatchThreeStateNormalization spec §5.2 三态表：header null/缺省 ⇒
// ""（Del）；格式层 "" 覆盖全局层 Set ⇒ Del；formats.<k> null/空对象 ⇒ 删层；
// 空 headers/formats/patch 或全空 ⇒ nil。
func TestRequestPatchThreeStateNormalization(t *testing.T) {
	// ① header 值 JSON null 解码为零值 "" ⇒ Del（与空串同义）。
	p, err := ParseRequestPatch(json.RawMessage(`{"headers":{"X-Del":null,"X-Set":"v"}}`))
	require.NoError(t, err)
	require.Equal(t, "", p.Headers["X-Del"])
	require.Equal(t, "v", p.Headers["X-Set"])

	// ② 格式层同键 ""（全局层是 Set）⇒ 格式层覆盖为 Del（跨层覆盖语义）。
	p, err = ParseRequestPatch(json.RawMessage(`{"headers":{"X-A":"g"},"formats":{"anthropic":{"headers":{"X-A":""}}}}`))
	require.NoError(t, err)
	require.Equal(t, "", p.Formats[UpstreamPathAnthropic].Headers["X-A"])

	// ③ formats.<k> null 或 {} ⇒ 删除该层；空 headers ⇒ nil。
	p, err = ParseRequestPatch(json.RawMessage(`{"headers":{},"formats":{"openai-chat":null,"anthropic":{}}}`))
	require.NoError(t, err)
	require.Nil(t, p, "空层必须归一到 nil（全空 ⇒ 未配置）")

	// ④ 只有一层非空时另一层为 nil。
	p, err = ParseRequestPatch(json.RawMessage(`{"headers":{},"formats":{"anthropic":{"delete":["a.b"]}}}`))
	require.NoError(t, err)
	require.Nil(t, p.Headers)
	require.Len(t, p.Formats, 1)

	// ⑤ 输入边界：nil / 空 / null / 空对象 ⇒ nil, nil。
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("  "), json.RawMessage("null"), json.RawMessage("{}")} {
		p, err := ParseRequestPatch(raw)
		require.NoError(t, err)
		require.Nil(t, p, string(raw))
	}
}

// TestRequestPatchInvalidHeaderNameValue 头名/值 wire 合法性（pkg/httpx 单源）。
func TestRequestPatchInvalidHeaderNameValue(t *testing.T) {
	invalid := []string{
		`{"headers":{"Bad Name":"v"}}`,
		`{"headers":{"Bad:Name":"v"}}`,
		`{"headers":{"X\tY":"v"}}`,
		`{"headers":{"":"v"}}`,
		`{"headers":{"X-A":"a\r\nb"}}`,
		`{"headers":{"X-A":"a\u0000b"}}`,
		`{"headers":{"X-A":"a\u007fb"}}`,
		`{"formats":{"anthropic":{"headers":{"X-A":"a\nb"}}}}`,
	}
	for _, raw := range invalid {
		_, err := ParseRequestPatch(json.RawMessage(raw))
		require.Error(t, err, raw)
	}
	// 值允许 TAB、UTF-8 高位字节与 {api_key} 占位符（占位符本轮不展开，§2.4/M7）。
	p, err := ParseRequestPatch(json.RawMessage(`{"headers":{"Authorization":"Bearer {api_key}","X-Utf8":"caf\u00e9","X-Tab":"a\tb"}}`))
	require.NoError(t, err)
	require.Equal(t, "Bearer {api_key}", p.Headers["Authorization"])
	require.Equal(t, "café", p.Headers["X-Utf8"])
	require.Equal(t, "a\tb", p.Headers["X-Tab"])
}

// TestRequestPatchInvalidPaths set/delete 的 path 文法校验（保存期 400）。
func TestRequestPatchInvalidPaths(t *testing.T) {
	invalid := []string{
		`{"formats":{"anthropic":{"set":{"":1}}}}`,
		`{"formats":{"anthropic":{"set":{"a.":1}}}}`,
		`{"formats":{"anthropic":{"set":{".a":1}}}}`,
		`{"formats":{"anthropic":{"set":{"a..b":1}}}}`,
		`{"formats":{"anthropic":{"set":{"a\\":1}}}}`,
		`{"formats":{"anthropic":{"set":{"a*b":1}}}}`,
		`{"formats":{"anthropic":{"set":{"a[0]":1}}}}`,
		`{"formats":{"anthropic":{"delete":["a|b"]}}}`,
		`{"formats":{"anthropic":{"delete":["a#"]}}}`,
	}
	for _, raw := range invalid {
		_, err := ParseRequestPatch(json.RawMessage(raw))
		require.Error(t, err, raw)
	}
}

// TestValidateSJSONPath path 文法正反例（sjson 简单路径：点分键 + 数组下标 +
// 基本转义；不允许空段/歧义转义/复杂表达式）。
func TestValidateSJSONPath(t *testing.T) {
	valid := []string{
		"model", "stream", "metadata.call_source", "input.0.content",
		"tools.-1", "a.b.c", "messages.0.content.1.text", ":1", ":a.b",
		`a\.b`, // 转义 '.' ⇒ 字面键 "a.b"
		`a\\b`, // 转义反斜杠 ⇒ 字面键 "a\b"
		`d\.e\.f`,
		"中文.字段", "a-b_c", "x y", "0", "-1",
	}
	for _, path := range valid {
		require.NoError(t, ValidateSJSONPath(path), "合法 path %q", path)
	}
	// raw string 直接写字面路径：第一个是「以转义符结尾 a\」，第二个是「转义了
	// 一个反斜杠、随后又以转义符结尾 a\\\」（两者都是 sjson 会静默吞字符的歧义
	// 转义）。
	invalid := []string{
		"", ".", "a.", ".a", "a..b", `a\`, `a\\\`,
		"a*b", "a?b", "a|b", "a#b", "a@b", "a[0]", "a[]", ":",
		"a\x1bb",
	}
	for _, path := range invalid {
		require.Error(t, ValidateSJSONPath(path), "非法 path %q", path)
	}
}

// TestRequestPatchIsEmpty nil / 空两层判定（nil 安全）。
func TestRequestPatchIsEmpty(t *testing.T) {
	var p *RequestPatch
	require.True(t, p.IsEmpty())
	require.True(t, (&RequestPatch{}).IsEmpty())
	require.True(t, (&RequestPatch{Headers: map[string]string{}, Formats: map[UpstreamPathKey]FormatPatch{}}).IsEmpty())
	require.False(t, (&RequestPatch{Headers: map[string]string{"X-A": ""}}).IsEmpty(), "空值也是有效配置（Del）")
	require.False(t, (&RequestPatch{Formats: map[UpstreamPathKey]FormatPatch{UpstreamPathChat: {Delete: []string{"a"}}}}).IsEmpty())
}
