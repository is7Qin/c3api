package protoconv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// (h) 的 oracle 表消费者：schema_instances.tsv = `case_name / 实例 JSON / must_accept|must_reject`。
// 说明：`encoding/json` 对重复键天然是**末个胜**，正好就是 spec 要求的
// normalizeDup(S)（独立实现，不与 T 共用任何辅助函数）。

type oracleRow struct {
	name string
	inst any
	mode string
}

func loadOracle(t *testing.T) map[string]oracleRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "schema_instances.tsv"))
	require.NoError(t, err)
	out := map[string]oracleRow{}
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || line == "\r" {
			continue
		}
		f := splitTSVLine(line)
		require.Len(t, f, 3, "第 %d 行必须 3 列", i+1)
		row := oracleRow{name: f[0], mode: f[2]}
		if f[1] != "-" {
			require.NoError(t, json.Unmarshal([]byte(f[1]), &row.inst), "第 %d 行实例必须是合法 JSON", i+1)
		}
		out[f[0]] = row
	}
	return out
}

func TestOracleInstances(t *testing.T) {
	oracle := loadOracle(t)
	accepted, rejected := 0, 0
	for _, r := range loadSchemaCases(t) {
		row, ok := oracle[r.name]
		require.True(t, ok, "oracle 缺 %s", r.name)
		if row.mode == "-" {
			require.Nil(t, row.inst, "%s: 无实例的行第 2 列必须是 -", r.name)
			continue
		}
		out, deg := messSchema(r.src)
		if deg == degBail {
			continue // 兜底产物是全集的，由 (b′) 金格锁定
		}
		var outDoc any
		require.NoError(t, json.Unmarshal([]byte(out), &outDoc), "%s: 产物必须是合法 JSON", r.name)
		evT := newEvaluator(outDoc)
		got := evT.valid(outDoc, row.inst)
		switch row.mode {
		case "must_accept":
			accepted++
			// 语义：**产物**必须接受该实例。源侧是否接受不在本表口径内
			// （oneOf 两个都匹配时源侧本就拒绝，那是 I1 的域，由 TestI1Superset 覆盖）。
			require.True(t, got, "H[case=%s]: 实例 %v 必须被产物接受\n产物=%s", r.name, row.inst, out)
		case "must_reject":
			rejected++
			require.False(t, got, "H[case=%s]: 实例 %v 必须被产物拒绝\n产物=%s", r.name, row.inst, out)
		default:
			t.Fatalf("%s: 未知模式 %q", r.name, row.mode)
		}
	}
	// 防恒绿：两类实例都必须真被消费（momus-r2 I-16：表不能是空壳）
	require.Greater(t, accepted, 30, "must_accept 实例数太少，oracle 形同虚设")
	require.Greater(t, rejected, 3, "must_reject 实例数太少，oracle 抓不到收窄")
}
