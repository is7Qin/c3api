package protoconv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// tsvDialectAnchors 读 dialect_anchors.tsv（独立 loader，不复用 content-model 的 anchors.tsv）。
func tsvDialectAnchors(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "spec", "dialect_anchors.tsv"))
	require.NoError(t, err)
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		require.Len(t, f, 2, "dialect_anchors.tsv 每行两列：field/value")
		out[f[0]] = f[1]
	}
	return out
}

// (e)① dialect 常量 ↔ dialect_anchors.tsv 双向机检。
func TestDialectAnchors(t *testing.T) {
	anchors := tsvDialectAnchors(t)
	want := map[string]string{
		"forbiddenRootKeys":   strings.Join(forbiddenRootKeys[:], ","),
		"requireRootType":     requireRootType,
		"localRefOnly":        "true",
		"maxRefHops":          "32",
		"maxDescriptionBytes": "8192",
		"refKeys":             strings.Join(refKeys[:], ","),
		"bailKeys":            strings.Join(bailKeys[:], ","),
		"defsTables":          "$defs,definitions",
		"degenerateHint":      degenerateHint,
		"bailHint":            bailHint,
	}
	for k, v := range want {
		got, ok := anchors[k]
		require.True(t, ok, "dialect_anchors.tsv 缺字段 %s", k)
		require.Equal(t, v, got, "字段 %s 与代码不一致", k)
	}
	// 反向：TSV 里的字段都必须在 want 里（schema_cases_sha256 是哈希槽，单独由 (e)⑤ 校验）。
	for k := range anchors {
		if k == "schema_cases_sha256" {
			continue
		}
		_, ok := want[k]
		require.True(t, ok, "TSV 里的 %s 没有对应代码常量", k)
	}
}

// (e)②③ 四个映射的 mode 与代码内派生一致。
func TestSchemaModeMaps(t *testing.T) {
	require.NoError(t, validateToolsMaps([]*toolsFieldMap{
		&toolsMessFromChat, &toolsMessFromResp, &toolsRespFromMess, &toolsRespFromChat,
	}))
	seen := map[direction]int{}
	for _, m := range []*toolsFieldMap{
		&toolsMessFromChat, &toolsMessFromResp, &toolsRespFromMess, &toolsRespFromChat,
	} {
		seen[m.dir]++
		require.NotEqual(t, schemaModeUnknown, m.mode)
		require.Equal(t, schemaModeFor(m.dir), m.mode)
	}
	require.Equal(t, map[direction]int{
		dirChatToMess: 1, dirRespToMess: 1, dirMessToResp: 1, dirChatToResp: 1,
	}, seen)
	// 未知方向必须派生成 Unknown（fail-fast 的前提）。
	require.Equal(t, schemaModeUnknown, schemaModeFor(dirRespToMessResp))
}

// missMessSchema 便捷：跑一次 mess 方言求值。
func messSchema(raw string) (string, degenerateKind) {
	str, b, deg := evalMessSchema(gjson.Parse(raw))
	if b == nil {
		return str, deg
	}
	return string(b), deg
}

// (b′) 9b 安全阀的三行 golden。
func TestBailValve(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		deg  degenerateKind
	}{
		{
			// ① 合法根 $defs 引用：不兜底，引用原字节保留。
			name: "refs_root_defs_pass",
			in:   `{"type":"object","$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}}}},"oneOf":[{"$ref":"#/$defs/P"}]}`,
			want: `{"type":"object","properties":{"q":{"type":"string"}},"$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}}}}}`,
			deg:  degSimplify,
		},
		{
			// ② 分支自带 $defs、引用指向它 ⇒ 产物悬空 ⇒ 兜底。
			name: "refs_branch_defs_bail",
			in:   `{"type":"object","oneOf":[{"type":"object","$defs":{"P":{"type":"object"}},"properties":{"a":{"$ref":"#/$defs/P"}}}]}`,
			want: `{"type":"object"}`,
			deg:  degBail,
		},
		{
			// ③ 子路径引用（非“恰好一个名字段”）⇒ 兜底。
			name: "refs_subpath_bail",
			in:   `{"type":"object","$defs":{"H":{"properties":{"x":{"type":"object"}}}},"properties":{"a":{"$ref":"#/$defs/H/properties/x"}},"oneOf":[{"type":"object"}]}`,
			want: `{"type":"object"}`,
			deg:  degBail,
		},
		{
			// 名字含 % ⇒ 兜底（URI percent 解码会把名字改掉）。
			name: "refs_percent_bail",
			in:   `{"type":"object","$defs":{"A%20B":{"type":"object"}},"properties":{"p":{"$ref":"#/$defs/A%20B"}},"oneOf":[{"type":"object"}]}`,
			want: `{"type":"object"}`,
			deg:  degBail,
		},
		{
			// 出现 $id ⇒ 兜底（基址重置）。
			name: "refs_id_bail",
			in:   `{"type":"object","oneOf":[{"type":"object"}],"properties":{"a":{"$id":"https://x/y","type":"object"}}}`,
			want: `{"type":"object"}`,
			deg:  degBail,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, deg := messSchema(c.in)
			require.Equal(t, c.want, got)
			require.Equal(t, c.deg, deg)
		})
	}
}

// (j)① schema 级幂等：绕开 P_in 短路，直接对产物再跑一次 T。
func TestIdempotentSchema(t *testing.T) {
	inputs := []string{
		`{"type":"object","$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}}}},"oneOf":[{"$ref":"#/$defs/P"}]}`,
		`{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"number"}}}]}`,
		`{"type":"object","oneOf":[{"type":"object","$defs":{"P":{"type":"object"}},"properties":{"a":{"$ref":"#/$defs/P"}}}]}`,
		`{"type":"object","enum":["x"]}`,
	}
	for _, in := range inputs {
		first, _ := messSchema(in)
		second, _ := messSchema(first)
		require.Equal(t, first, second, "T(T(x)) 必须等于 T(x)；input=%s", in)
	}
}

// (k) 工具计数守恒 + 内置工具不输出。
func TestToolsCountPreserved(t *testing.T) {
	tools := gjson.Parse(`[
		{"type":"function","function":{"name":"a","parameters":{"type":"object","oneOf":[{"type":"object"}]}}},
		{"type":"web_search"},
		{"type":"function","function":{"name":"b","parameters":{"type":"object"}}},
		{"type":"function"}
	]`)
	out := appendToolsMapped(nil, tools, toolsMessFromChat)
	got := gjson.Parse("[" + string(out) + "]")
	require.True(t, got.IsArray())
	require.Len(t, got.Array(), 2, "只有两个合法 function 工具被写出")
	require.NotContains(t, string(out), "web_search")
}

// (l) 产物引用可解析（作用域：走 T 的产物）。判据与 9b 独立实现（此处直接用正则复核）。
func TestProductRefsResolvable(t *testing.T) {
	inputs := []string{
		`{"type":"object","$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}}}},"oneOf":[{"$ref":"#/$defs/P"}]}`,
		`{"type":"object","oneOf":[{"type":"object","$defs":{"P":{"type":"object"}},"properties":{"a":{"$ref":"#/$defs/P"}}}]}`,
	}
	for _, in := range inputs {
		out, _ := messSchema(in)
		tables := collectTables([]byte(out))
		ok := true
		var walk func(lo, hi int)
		walk = func(lo, hi int) {
			if !ok || lo >= hi || hi > len(out) {
				return
			}
			switch out[lo] {
			case '{':
				scanObject([]byte(out), lo, hi, func(kr []byte, vl, vh int) bool {
					name := decodeRawKey(kr)
					for _, rk := range refKeys {
						if name == rk {
							if !refOK([]byte(out)[vl:vh], tables) {
								ok = false
								return false
							}
							return true
						}
					}
					walk(vl, vh)
					return ok
				})
			case '[':
				scanArray([]byte(out), lo, hi, func(vl, vh int) bool {
					walk(vl, vh)
					return ok
				})
			}
		}
		walk(0, len(out))
		require.True(t, ok, "产物引用必须可解析：%s ⇒ %s", in, out)
	}
}

// (q) 非对象实例域：改写为 object 而不是 DROP。
func TestNonObjectDomain(t *testing.T) {
	out, _ := messSchema(`{"type":"string","properties":{"a":{"type":"string"}}}`)
	require.Equal(t, `{"type":"object","properties":{"a":{"type":"string"}}}`, out)
	require.Equal(t, "object", gjson.Parse(out).Get("type").Str, "根 type 必须被改写成 object")
	require.True(t, strings.HasPrefix(out, `{"type":"object"`))
}

// 9b 判据的边界（表格驱动，锁白名单语义）。
func TestRefNameWhitelist(t *testing.T) {
	for _, ok := range []string{"P", "a_b-c.d", "A1", strings.Repeat("x", 128)} {
		require.True(t, refNameOK(ok), ok)
	}
	for _, bad := range []string{"", "a/b", "a~1b", "a%20b", "a b", "名字", strings.Repeat("x", 129), "a#b"} {
		require.False(t, refNameOK(bad), bad)
	}
}
