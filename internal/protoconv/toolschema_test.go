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
		"maxValveDepth":       "256",
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
			deg:  degNone,
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
			in:   `{"type":"object","$defs":{"H":{"properties":{"x":{"type":"object"}}}},"oneOf":[{"type":"object","properties":{"a":{"$ref":"#/$defs/H/properties/x"}}}],"properties":{"a":{"$ref":"#/$defs/H/properties/x"}}}`,
			want: `{"type":"object"}`,
			deg:  degBail,
		},
		{
			// 名字含 % ⇒ 兜底（URI percent 解码会把名字改掉）。
			name: "refs_percent_bail",
			in:   `{"type":"object","$defs":{"A%20B":{"type":"object"}},"oneOf":[{"type":"object","properties":{"p":{"$ref":"#/$defs/A%20B"}}}],"properties":{"p":{"$ref":"#/$defs/A%20B"}}}`,
			want: `{"type":"object"}`,
			deg:  degBail,
		},
		{
			// 出现 $id ⇒ 兜底（基址重置）。
			name: "refs_id_bail",
			in:   `{"type":"object","$id":"https://x/y","oneOf":[{"type":"object","properties":{"a":{"type":"object"}}}]}`,
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

// (g) 基准夹具与目标表的存在性 / 完整性（数值断言由 M0 记录 + 棘轮约定承担）。
func TestBenchFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "spec", "protoconv-toolschema-bench-M0.txt"))
	require.NoError(t, err)
	txt := string(raw)
	for _, must := range []string{"goos=", "goarch=", "cpu=", "pkg=", "-count=", "src_sha256 "} {
		require.Contains(t, txt, must, "M0 记录缺 %s", must)
	}
	targets, err := os.ReadFile(filepath.Join("testdata", "spec", "bench_targets.tsv"))
	require.NoError(t, err)
	want := []string{
		"BenchmarkConvertRequestRespToMessTools20",
		"BenchmarkToolsHandling",
		"BenchmarkMessSchemaScan",
		"BenchmarkMessSchemaFlatten",
		"BenchmarkToolsRatio",
	}
	for _, b := range want {
		require.Contains(t, string(targets), b, "bench_targets.tsv 缺 %s", b)
		require.Contains(t, txt, b, "M0 记录缺 %s 的实测行", b)
	}
}

// pInFast 与 scanRoot().pIn() 必须逐输入一致（快路径不许更宽松）。
func TestPInFastParity(t *testing.T) {
	inputs := []string{
		`{}`, `{"properties":{"a":{}}}`, `{"title":"t"}`,
		`{"$schema":"http://x","properties":{}}`, `{"Type":"object","properties":{"a":{}}}`,
		`{"type":"object"}`, `{"type":"object","properties":{}}`,
		`{"type":"string"}`, `{"type":["object","null"]}`,
		`{"type":"object","oneOf":[]}`, `{"type":"object","enum":[]}`,
		`{"type":"object","$ref":"#/$defs/P"}`,
		`{"type":"object","type":"string"}`,
		`{"type":"object","properties":{},"properties":{}}`,
		`{"type":"object","title":"t","properties":{"a":{}}}`,
	}
	for _, in := range inputs {
		src := gjson.Parse(in)
		raw := []byte(in)
		require.Equal(t, scanRoot(raw).pIn(raw), pInFast(src), "判定不一致：%s", in)
	}
}

// 9b 安全阀的递归必须有深度上限（深嵌套输入不许把栈打爆）。
func TestValveDepthCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"type":"object","oneOf":[{"type":"object","properties":{"x":`)
	depth := maxValveDepth + 50 // 超过上限 ⇒ 兜底，但不许崩
	for i := 0; i < depth; i++ {
		b.WriteString(`{"n":`)
	}
	b.WriteString(`1`)
	for i := 0; i < depth; i++ {
		b.WriteString(`}`)
	}
	b.WriteString(`}}]}`)
	src := b.String()
	out, deg := messSchema(src)
	// 不崩即可；深嵌套超出可证明范围时按兜底处理
	require.NotEmpty(t, out)
	require.Contains(t, []degenerateKind{degNone, degSimplify, degBail}, deg)
}

// 深嵌套但无引用的 schema 不应被兜底（阀只对引用与 $id 敏感）。
func TestDeepNestingNoRef(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"type":"object","oneOf":[{"type":"object","properties":{"x":`)
	depth := 80
	for i := 0; i < depth; i++ {
		b.WriteString(`{"n":`)
	}
	b.WriteString(`{"type":"string"}`)
	for i := 0; i < depth; i++ {
		b.WriteString(`}`)
	}
	b.WriteString(`}}]}`)
	out, deg := messSchema(b.String())
	require.NotEqual(t, degBail, deg, "无引用的深嵌套不该兜底")
	require.Contains(t, out, `"type":"string"`)
}
