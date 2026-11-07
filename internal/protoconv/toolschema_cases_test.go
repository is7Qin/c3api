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

func loadSchemaCases(t *testing.T) []caseRow {
	t.Helper()
	raw, err := os.ReadFile(schemaCasesPath)
	require.NoError(t, err)
	var out []caseRow
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
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

// (a) 输出侧 P_out 全称断言：out-1..out-5。
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
	obj.ForEach(func(k, _ gjson.Result) bool {
		if _, ok := seen[k.Str]; ok {
			dup = true
		}
		seen[k.Str] = struct{}{}
		return true
	})
	require.False(t, dup, "%s: out-5 不得有重复根键", name)
	for k := range seen {
		_, bad := forbidden[k]
		require.False(t, bad, "%s: out-4 根键 %s 在禁键集里", name, k)
	}
	got := obj.Get("type")
	require.Equal(t, gjson.String, got.Type, "%s: out-3 根 type 必须是字符串", name)
	require.Equal(t, "object", got.Str, "%s: out-3 根 type 必须是 object", name)
}

// (b)(a)(p)(m)：逐行字节断言 + 结构断言。
func TestSchemaCases(t *testing.T) {
	rows := loadSchemaCases(t)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, deg := messSchema(r.src)
			require.Equal(t, r.want, got, "%s（%s）", r.name, r.note)
			assertPOut(t, r.name, got)
			// (b) 第 4 列：属性名集合必须与产物一致。
			var props []string
			gjson.Parse(got).Get("properties").ForEach(func(k, _ gjson.Result) bool {
				props = append(props, k.Str)
				return true
			})
			require.ElementsMatch(t, r.propSet, props, "%s: 属性名集合")
			// (b) 第 5 列：reason 集合（只看退化种类这一层，动作级 reason 由 (n) 覆盖）。
			require.Len(t, r.reasons, boolToLen(deg != degNone), "%s: reason 列与退化种类不符", r.name)
			if deg == degBail {
				require.Contains(t, r.reasons, "dangling-after-flatten", "%s", r.name)
			}
			// (b) 第 6 列：备注必须以 r<N>: 开头。
			require.Regexp(t, `^r\d+:`, r.note, "%s: 备注列格式", r.name)
		})
	}
	require.GreaterOrEqual(t, len(rows), 50, "(b) 用例行数下限")
}

func boolToLen(b bool) int {
	if b {
		return 1
	}
	return 0
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
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		require.Len(t, f, 3, "第 %d 行必须是 3 列", i+1)
		got[f[0]] = struct{}{}
	}
	require.Len(t, got, len(rows), "instances 与 cases 的用例名必须一一对应")
	for _, r := range rows {
		_, ok := got[r.name]
		require.True(t, ok, "instances 缺 %s", r.name)
	}
}
