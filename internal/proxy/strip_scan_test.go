// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// scanTopLevelKeys / scanTools / decodeUnicodeEscapes 单元测试
// （2026-08-12-strip-optimize-spec v5 测试节：键定位/区间/转义/嵌套/预筛
// 短路/键名干扰/\u 必剥）。
// 注意：本文件反斜杠字节经脚本注入（chr(92)），避免源码转义歧义。
// ---------------------------------------------------------------------------

func TestScanTopLevelKeys(t *testing.T) {
	// assertKey 校验 keyStart 指向 "tools"/"tool_choice" 键名、值区间字节、
	// 逗号位置语义、delRange 拼接结果。
	assertKey := func(t *testing.T, body []byte, r topLevelRange, key, val string, commaBefore, commaAfter int, afterDel string) {
		t.Helper()
		require.Equal(t, key, string(body[r.keyStart:r.keyStart+len(key)]), "键名精确匹配")
		require.Equal(t, val, string(body[r.valStart:r.valEnd]), "值区间裸字节")
		require.Equal(t, commaBefore, r.commaBefore, "前导逗号位置")
		require.Equal(t, commaAfter, r.commaAfter, "后随逗号位置")
		if r.commaBefore >= 0 {
			require.Equal(t, byte(','), body[r.commaBefore], "前导逗号为 ','")
		}
		if r.commaAfter >= 0 {
			require.Equal(t, byte(','), body[r.commaAfter], "后随逗号为 ','")
		}
		s, e := r.delRange()
		require.Equal(t, afterDel, string(append(append([]byte{}, body[:s]...), body[e:]...)), "delRange 删键拼接")
	}

	t.Run("紧凑双键", func(t *testing.T) {
		body := []byte(`{"model":"m","tools":[{"type":"image_generation_tool"}],"tool_choice":{"type":"image_generation_tool"}}`)
		tools, tc, toolsOK, tcOK := scanTopLevelKeys(body)
		require.True(t, toolsOK)
		require.True(t, tcOK)
		assertKey(t, body, tools, `"tools"`, `[{"type":"image_generation_tool"}]`, 12, 55,
			`{"model":"m","tool_choice":{"type":"image_generation_tool"}}`)
		assertKey(t, body, tc, `"tool_choice"`, `{"type":"image_generation_tool"}`, 55, -1,
			`{"model":"m","tools":[{"type":"image_generation_tool"}]}`)
	})

	t.Run("冒号前空白变体", func(t *testing.T) {
		// bytes.Index 不可靠的探针实证形态：`"tools" : [` 必须命中。
		body := []byte(`{"model":"m" , "tools" : [1,2] , "b":true}`)
		tools, _, toolsOK, tcOK := scanTopLevelKeys(body)
		require.True(t, toolsOK)
		require.False(t, tcOK)
		// 删键区间 [commaBefore, valEnd)：逗号前/值后各留一个空白（合法 JSON）。
		assertKey(t, body, tools, `"tools"`, `[1,2]`, 13, 31, `{"model":"m"  , "b":true}`)
	})

	t.Run("键名前缀干扰", func(t *testing.T) {
		body := []byte(`{"tools_extra":1,"x_tools":2,"tools":[3]}`)
		tools, _, toolsOK, _ := scanTopLevelKeys(body)
		require.True(t, toolsOK)
		assertKey(t, body, tools, `"tools"`, `[3]`, 28, -1, `{"tools_extra":1,"x_tools":2}`)
	})

	t.Run("字符串内容含 tools 字样不误定位", func(t *testing.T) {
		// 值字符串含转义引号 + `"tools"` 字样：必须整体跳过。
		body := []byte(`{"a":"say \"tools\" here","tools":[1]}`)
		tools, _, toolsOK, _ := scanTopLevelKeys(body)
		require.True(t, toolsOK)
		assertKey(t, body, tools, `"tools"`, `[1]`, 25, -1, `{"a":"say \"tools\" here"}`)
	})

	t.Run("嵌套值跳过", func(t *testing.T) {
		// 嵌套对象内出现 "tools"/"tool_choice" 键：不得误定位。
		body := []byte(`{"input":{"tools":[1],"tool_choice":{"type":"x"}},"tools":[2]}`)
		tools, _, toolsOK, tcOK := scanTopLevelKeys(body)
		require.True(t, toolsOK)
		require.False(t, tcOK, "嵌套 tool_choice 不误定位")
		assertKey(t, body, tools, `"tools"`, `[2]`, 49, -1, `{"input":{"tools":[1],"tool_choice":{"type":"x"}}}`)
	})

	t.Run("重复键取最后出现", func(t *testing.T) {
		// 对齐 json.Unmarshal map 语义：重复键后者胜。
		body := []byte(`{"tools":[1],"tools":[2]}`)
		tools, _, toolsOK, _ := scanTopLevelKeys(body)
		require.True(t, toolsOK)
		assertKey(t, body, tools, `"tools"`, `[2]`, 12, -1, `{"tools":[1]}`)
	})

	t.Run("顶层非对象", func(t *testing.T) {
		for _, body := range []string{`[{"tools":[1]}]`, `"tools"`, `null`, `123`, `[]`} {
			_, _, toolsOK, tcOK := scanTopLevelKeys([]byte(body))
			require.False(t, toolsOK, "顶层非对象 → tools 缺省（原样转发语义）：%s", body)
			require.False(t, tcOK, "顶层非对象 → tool_choice 缺省：%s", body)
		}
	})

	t.Run("三位置逗号删除", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			want string // delRange 后结果
		}{
			{name: "唯一键", body: `{"tools":[1]}`, want: `{}`},
			{name: "首键", body: `{"tools":[1],"model":"m"}`, want: `{"model":"m"}`},
			{name: "末键", body: `{"model":"m","tools":[1]}`, want: `{"model":"m"}`},
			{name: "中间键", body: `{"a":1,"tools":[1],"b":2}`, want: `{"a":1,"b":2}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tools, _, toolsOK, _ := scanTopLevelKeys([]byte(tc.body))
				require.True(t, toolsOK)
				s, e := tools.delRange()
				got := append(append([]byte{}, tc.body[:s]...), tc.body[e:]...)
				require.Equal(t, tc.want, string(got))
			})
		}
	})
}

func TestScanTools(t *testing.T) {
	// collect 迭代收集所有元素视图。
	collect := func(t *testing.T, raw []byte) []ToolView {
		t.Helper()
		var vs []ToolView
		for v := range scanTools(raw) {
			vs = append(vs, v)
		}
		return vs
	}

	t.Run("紧凑数组与空白冒号", func(t *testing.T) {
		// 元素 1 含 "image" 字样触发提取路径（预筛前置），键值带冒号前空白。
		raw := []byte(`[{"type" : "function","name":"shell","description":"image helper"},{"type":"image_generation_tool"}]`)
		vs := collect(t, raw)
		require.Len(t, vs, 2)
		require.Equal(t, `{"type" : "function","name":"shell","description":"image helper"}`, string(vs[0].Raw))
		require.Equal(t, "function", string(vs[0].Type))
		require.Equal(t, "shell", string(vs[0].Name))
		require.Equal(t, "image_generation_tool", string(vs[1].Type))
	})

	t.Run("缩进格式元素去空白", func(t *testing.T) {
		raw := []byte("[\n\t{\"type\": \"function\"},\n  {\"type\": \"image_edits\"}\n]")
		vs := collect(t, raw)
		require.Len(t, vs, 2)
		require.Equal(t, `{"type": "function"}`, string(vs[0].Raw), "元素 Raw 去除前导/尾随空白")
		require.Equal(t, `{"type": "image_edits"}`, string(vs[1].Raw))
	})

	t.Run("空数组", func(t *testing.T) {
		require.Len(t, collect(t, []byte(`[]`)), 0)
		require.Len(t, collect(t, []byte(`[ ]`)), 0)
		require.Len(t, collect(t, []byte("[\n\t]")), 0)
	})

	t.Run("标量元素零键提取", func(t *testing.T) {
		vs := collect(t, []byte(`[null,123,"str"]`))
		require.Len(t, vs, 3)
		require.Equal(t, "null", string(vs[0].Raw))
		require.Equal(t, "123", string(vs[1].Raw))
		require.Equal(t, `"str"`, string(vs[2].Raw))
		for i := range vs {
			require.Nil(t, vs[i].Type, "双不命中元素 → 零键提取")
		}
	})

	t.Run("字符串转义与含逗号字符串", func(t *testing.T) {
		// 元素 1/2 经 description 含 "image" 触发提取；元素 3 含 \u 触发。
		raw := []byte(`[{"type":"a\"b","description":"image"},{"type":"c\\d","description":"image"},{"type":"xX","description":"image"},"a,b"]`)
		vs := collect(t, raw)
		require.Len(t, vs, 4, "字符串元素含逗号不切分")
		require.Equal(t, `a\"b`, string(vs[0].Type), "\" 转义保留原样")
		require.Equal(t, `c\\d`, string(vs[1].Type), "\\ 转义保留原样（仅 \\uXXXX 解码，其余转义保留裸字节——比较语义等价）")
		require.Equal(t, "xX", string(vs[2].Type), "提取路径正常（description 触发预筛）")
		require.Equal(t, `"a,b"`, string(vs[3].Raw), "字符串元素整体跳过（含逗号）")
		require.Nil(t, vs[3].Type)
	})

	t.Run("嵌套对象数组", func(t *testing.T) {
		raw := []byte(`[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}],"parameters":{"x":[1,2]}}]`)
		vs := collect(t, raw)
		require.Len(t, vs, 1)
		require.Equal(t, "namespace", string(vs[0].Type), "嵌套 type 不提取（仅顶层键）")
		require.Equal(t, "image_gen", string(vs[0].Name))
		require.Nil(t, vs[0].Namespace)
	})

	t.Run("键名干扰", func(t *testing.T) {
		raw := []byte(`[{"type2":"image_generation_tool","type ":"image_edits","type" : "namespace","name":"image_gen"}]`)
		vs := collect(t, raw)
		require.Len(t, vs, 1)
		require.Equal(t, "namespace", string(vs[0].Type), "type2/type 空格干扰不误命中")
		require.Equal(t, "image_gen", string(vs[0].Name))
	})

	t.Run("预筛短路零键提取", func(t *testing.T) {
		raw := []byte(`[{"type":"function","name":"shell","parameters":{"type":"object","additionalProperties":false}}]`)
		vs := collect(t, raw)
		require.Len(t, vs, 1)
		require.Nil(t, vs[0].Type, "无 image 无 \\u → 零键提取（仅 Raw）")
		require.Nil(t, vs[0].Name)
	})

	t.Run("unicode 转义类型解码", func(t *testing.T) {
		// \u 转义工具标识：字节无 "image" 子串（i 未解码）→ 预筛 \u
		// 路径 → 解码后判定（与现状 gjson String() 等价）。
		raw := []byte(`[{"type":"\u0069mage_generation_tool","namespace":"image_gen"}]`)
		vs := collect(t, raw)
		require.Len(t, vs, 1)
		require.Equal(t, "image_generation_tool", string(vs[0].Type), "\\u0069 解码 = i")
		require.Equal(t, "image_gen", string(vs[0].Namespace))
	})

	t.Run("双反斜杠字面不解码", func(t *testing.T) {
		// JSON 文本 `"\\u0069mage_generation_tool"` = 字面反斜杠 + u0069：
		// 解码器 \\ 前缀感知防漏剥/错剥——Type 保留字面原样。
		raw := []byte(`[{"type":"\\u0069mage_generation_tool"}]`)
		vs := collect(t, raw)
		require.Len(t, vs, 1)
		require.Equal(t, `\u0069mage_generation_tool`, string(vs[0].Type), "\\\\u 字面不解码（保留原样）")
	})

	t.Run("break 提前终止", func(t *testing.T) {
		raw := []byte(`[{"type":"function"},{"type":"image_generation_tool"},{"type":"namespace"}]`)
		n := 0
		for v := range scanTools(raw) {
			if bytes.Equal(v.Type, typeImageGenTool) {
				break
			}
			n++
		}
		require.Equal(t, 1, n, "break 自动停止迭代")
	})
}

// ---------------------------------------------------------------------------
// scanKeys / parseStringValue 单元测试
// （2026-08-16-single-pass-parse-design：多键单遍收集——handleFormat 单遍
// 提取地基；重复键取首 / 缺键 nil / 嵌套不误定位 / 顶层非对象 false）。
// ---------------------------------------------------------------------------

func TestScanKeys(t *testing.T) {
	keys := [][]byte{streamKeyBytes, modelKeyBytes, serviceTierKeyBytes}
	collect := func(t *testing.T, body string) (vals [3][]byte, ok bool) {
		t.Helper()
		ok = scanKeys([]byte(body), keys, vals[:])
		return vals, ok
	}

	t.Run("三键任意顺序", func(t *testing.T) {
		// 键顺序与 keys 参数顺序相反——值按 keys 索引归位。
		vals, ok := collect(t, `{"service_tier":"fast","stream":true,"model":"gpt-4o","messages":[]}`)
		require.True(t, ok)
		require.Equal(t, "true", string(vals[0]), "stream 值区间裸字节")
		require.Equal(t, `"gpt-4o"`, string(vals[1]), "字符串值区间含引号（去引号归 parseStringValue）")
		require.Equal(t, `"fast"`, string(vals[2]))
	})

	t.Run("缺失键", func(t *testing.T) {
		vals, ok := collect(t, `{"model":"gpt-4o","messages":[]}`)
		require.True(t, ok)
		require.Nil(t, vals[0], "stream 缺失 → nil")
		require.Equal(t, `"gpt-4o"`, string(vals[1]))
		require.Nil(t, vals[2], "service_tier 缺失 → nil")
	})

	t.Run("嵌套同名键不误定位", func(t *testing.T) {
		body := `{"input":{"model":"nested","service_tier":"fast","stream":false},"model":"gpt-4o","stream":true}`
		vals, ok := collect(t, body)
		require.True(t, ok)
		require.Equal(t, "true", string(vals[0]), "嵌套 stream:false 不误定位")
		require.Equal(t, `"gpt-4o"`, string(vals[1]), "嵌套 model 不误定位")
		require.Nil(t, vals[2], "嵌套 service_tier 不误定位（顶层无）")
	})

	t.Run("字符串值区间含引号", func(t *testing.T) {
		// `"stream":"true"` 值区间 = `"true"`（含引号）——与字面 true 区分
		// （handleFormat 判定防字符串误判的语义前提）。
		vals, ok := collect(t, `{"stream":"true","model":null}`)
		require.True(t, ok)
		require.Equal(t, `"true"`, string(vals[0]))
		require.Equal(t, "null", string(vals[1]), "显式 null 字面原样捕获")
	})

	t.Run("重复键取首", func(t *testing.T) {
		// 与 gjson 路径查询首次命中语义一致（scanTopLevelKeys 取末次是 map
		// 语义——不同调用方不同约定）。
		vals, ok := collect(t, `{"model":"first","model":"second"}`)
		require.True(t, ok)
		require.Equal(t, `"first"`, string(vals[1]), "重复键取首次出现")
	})

	t.Run("顶层非对象", func(t *testing.T) {
		for _, body := range []string{`[{"model":"x"}]`, `"model"`, `null`, `123`, `[]`} {
			vals, ok := collect(t, body)
			require.False(t, ok, "顶层非对象 → ok=false：%s", body)
			for i := range vals {
				require.Nil(t, vals[i], "顶层非对象 → 全部缺省：%s", body)
			}
		}
	})

	t.Run("非法结构", func(t *testing.T) {
		// json.Valid 前置下不可达（防御性兜底）：截断/缺冒号等形态 → ok=false。
		for _, body := range []string{`{`, `{"model"`, `{"model":`, `{"model":"x`} {
			vals, ok := collect(t, body)
			require.False(t, ok, "非法结构 → ok=false：%s", body)
			for i := range vals {
				require.Nil(t, vals[i], "非法结构 → 全部缺省：%s", body)
			}
		}
		// 错误点在键收集之后（尾逗号截断）：已收集键保留（调用方忽略 ok——
		// 生产不可达：json.Valid 前置已 400）。
		vals, ok := collect(t, `{"model":1,`)
		require.False(t, ok)
		require.Equal(t, "1", string(vals[1]))
	})

	t.Run("空对象", func(t *testing.T) {
		vals, ok := collect(t, `{}`)
		require.True(t, ok, "空对象扫描成功（无键可收集）")
		for i := range vals {
			require.Nil(t, vals[i])
		}
	})

	t.Run("三键齐集即早退", func(t *testing.T) {
		// 遍历长度断言（大 body 键在前的场景）：三键位于对象前部、其后接
		// 非法结构——齐集早退 → true（尾随字节未触及）；无早退则继续遍历
		// 撞上结构非法 → false。task review Important（MB body 实测慢
		// 20-25%）修复的回归钉。
		vals, ok := collect(t, `{"stream":true,"model":"gpt-4o","service_tier":"auto",`)
		require.True(t, ok, "三键齐集早退：尾随非法结构不得被触及")
		require.Equal(t, "true", string(vals[0]))
		require.Equal(t, `"gpt-4o"`, string(vals[1]))
		require.Equal(t, `"auto"`, string(vals[2]))
	})

	t.Run("缺键不早退", func(t *testing.T) {
		// 齐集失败（缺键）→ 遍历至对象结束（尾随非法结构 → false）——早退
		// 只发生在全齐集，缺键路径行为不变。
		vals, ok := collect(t, `{"stream":true,"model":"gpt-4o",`)
		require.False(t, ok, "缺 service_tier → 不早退：尾随结构非法 → false")
		require.Equal(t, "true", string(vals[0]))
		require.Equal(t, `"gpt-4o"`, string(vals[1]))
		require.Nil(t, vals[2])
	})
}

func TestParseStringValue(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"缺键", "", "", true}, // gjson Null/缺失同零值语义放行
		{"null字面", "null", "", true},
		{"字符串", `"gpt-4o"`, "gpt-4o", true},
		{"空字符串", `""`, "", true},
		{"unicode转义", `"g\u002d4o"`, "g-4o", true}, // - 解码 = '-'（gjson String() 同款）
		{"双反斜杠字面", `"\\u0069"`, `\u0069`, true}, // JSON \\u = 字面反斜杠 + u0069，不解码
		{"数字", "123", "", false},
		{"布尔", "true", "", false},
		{"对象", "{}", "", false},
		{"数组", "[]", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseStringValue([]byte(tc.raw))
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, string(got))
		})
	}
}

func TestDecodeUnicodeEscapes(t *testing.T) {
	t.Run("无转义原切片直返", func(t *testing.T) {
		v := []byte("image_generation_tool")
		require.Same(t, &v[0], &decodeUnicodeEscapes(v)[0], "无 \\u → 零分配原切片")
	})

	// 注意：双引号 Go 字符串中 `\\` = 一个反斜杠字节——钉住 \uXXXX 转义字节。
	cases := []struct {
		in, want string
	}{
		{in: "image", want: "image"},       // 无 \u → 原样
		{in: "\\u0069", want: "i"},         // 单反斜杠 u0069 → 解码 i
		{in: "\\u0069\\u006d", want: "im"}, // 连续解码
		{in: "\\u0058", want: "X"},         // 大写 hex 数字
		{in: "\\u005A", want: "Z"},         // 大写 hex 字母（A-F 分支）
		{in: "\\u005c", want: "\\"},        // 0x5C = 反斜杠
		{in: "\\\\u0069", want: "\\u0069"}, // 双反斜杠字面不解码（\\ 前缀感知）
		{in: "\\\\\\u0069", want: "\\i"},   // 转义反斜杠 + 真转义
		{in: "a\\b", want: "a\\b"},         // 其他转义保留原样
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, string(decodeUnicodeEscapes([]byte(tc.in))), "in=%q", tc.in)
	}
}
