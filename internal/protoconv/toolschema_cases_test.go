package protoconv

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const schemaCasesPath = "testdata/schema_cases.tsv"

// caseRow (b) 的一行。
type caseRow struct {
	name    string
	src     string
	want    string
	propSet []string
	reasons []string
	note    string
}

// splitTSVLine 按 \t 切；顺带吃掉 CRLF 的 \r（M-6：仓库在 Windows 上开发）。
func splitTSVLine(line string) []string {
	line = strings.TrimSuffix(line, "\r")
	return strings.Split(line, "\t")
}

func loadSchemaCases(t *testing.T) []caseRow {
	t.Helper()
	raw, err := os.ReadFile(schemaCasesPath)
	require.NoError(t, err)
	var out []caseRow
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || line == "\r" {
			continue
		}
		f := splitTSVLine(line)
		require.Len(t, f, 6, "第 %d 行必须是 6 列", i+1)
		src, err := strconv.Unquote(f[1])
		require.NoError(t, err, "第 %d 行第 2 列必须可 Unquote", i+1)
		want, err := strconv.Unquote(f[2])
		require.NoError(t, err, "第 %d 行第 3 列必须可 Unquote", i+1)
		r := caseRow{name: f[0], src: src, want: want, note: f[5]}
		if f[3] != "-" {
			r.propSet = strings.Split(f[3], ",")
		}
		if f[4] != "-" {
			r.reasons = strings.Split(f[4], ",")
		}
		out = append(out, r)
	}
	require.NotEmpty(t, out)
	return out
}

// assertPOut (a) 输出侧 P_out 全称断言：out-1..out-5。
// 用 ForEach 遍历根键（不许 Get：Get 首胜看不见重复键）。
func assertPOut(t *testing.T, name, out string) {
	t.Helper()
	obj := gjson.Parse(out)
	require.True(t, obj.IsObject(), "%s: out-2 值必须是对象", name)
	seen := map[string]struct{}{}
	forbidden := map[string]struct{}{}
	for _, f := range forbiddenRootKeys {
		forbidden[f] = struct{}{}
	}
	var dup bool
	typeSeen := false
	typeIsObject := false
	obj.ForEach(func(k, v gjson.Result) bool {
		if _, ok := seen[k.Str]; ok {
			dup = true
		}
		seen[k.Str] = struct{}{}
		if k.Str == "type" {
			typeSeen = true
			typeIsObject = v.Type == gjson.String && v.Str == "object"
		}
		return true
	})
	require.False(t, dup, "%s: out-5 不得有重复根键", name)
	for k := range seen {
		_, bad := forbidden[k]
		require.False(t, bad, "%s: out-4 根键 %s 在禁键集里", name, k)
	}
	require.True(t, typeSeen, "%s: out-3 根 type 必须存在", name)
	require.True(t, typeIsObject, "%s: out-3 根 type 必须是 object", name)
}

// (b)(a)(p)(m)：逐行字节断言 + 结构断言 + reason 集合断言。
func TestSchemaCases(t *testing.T) {
	rows := loadSchemaCases(t)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			res := evalMessSchema(gjson.Parse(r.src))
			got := res.raw
			if got == "" {
				got = string(res.out)
			}
			require.Equal(t, r.want, got, "%s（%s）", r.name, r.note)
			assertPOut(t, r.name, got)
			// (p)① ⊇ 与 (p)② 下限：门禁只挡"丢属性"，不规定"不许有更多"（momus-r2 I-17）。
			var props []string
			gjson.Parse(got).Get("properties").ForEach(func(k, _ gjson.Result) bool {
				props = append(props, k.Str)
				return true
			})
			for _, want := range r.propSet {
				require.Contains(t, props, want, "P2[case=%s]: 属性名 %q 在产物里丢了", r.name, want)
			}
			require.GreaterOrEqual(t, len(props), len(r.propSet), "P2[case=%s]: 属性个数低于下限", r.name)
			// (b) 第 5 列：reason 集合**逐项**相等（C-4：只比长度会让内容写反也绿）
			require.ElementsMatch(t, r.reasons, res.reasons, "%s: reason 集合不符", r.name)
			// (b) 第 6 列：备注必须以 r<N>: 开头。
			require.Regexp(t, `^r\d+:`, r.note, "%s: 备注列格式", r.name)
			// missing 与 no-schema 的权威关系（(n)③ 的单向蕴含）。
			if res.missing {
				require.Contains(t, res.reasons, "no-schema", "%s: missing ⇒ 必须含 no-schema", r.name)
			}
		})
	}
	require.GreaterOrEqual(t, len(rows), 50, "(b) 用例行数下限")
}

// (e)⑤ schema_cases.tsv 的实际哈希必须与 anchors 里记录的一致。
func TestSchemaCasesHash(t *testing.T) {
	raw, err := os.ReadFile(schemaCasesPath)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	want := tsvDialectAnchors(t)["schema_cases_sha256"]
	require.NotEqual(t, "-", want, "anchors 里的 schema_cases_sha256 必须已回填")
	require.Equal(t, hex.EncodeToString(sum[:]), want)
}

// (h) 覆盖完整性：instances 的名字集合必须等于 cases 的名字集合。
func TestSchemaInstancesCoverage(t *testing.T) {
	rows := loadSchemaCases(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "schema_instances.tsv"))
	require.NoError(t, err)
	got := map[string]struct{}{}
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || line == "\r" {
			continue
		}
		f := splitTSVLine(line)
		require.Len(t, f, 3, "第 %d 行必须是 3 列", i+1)
		got[f[0]] = struct{}{}
	}
	require.Len(t, got, len(rows), "instances 与 cases 的用例名必须一一对应")
	for _, r := range rows {
		_, ok := got[r.name]
		require.True(t, ok, "instances 缺 %s", r.name)
	}
}
