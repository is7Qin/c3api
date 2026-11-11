package protoconv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// (m) `parameters` 缺失/null ⇒ mess 目标必含 input_schema 且满足 P_out
// ---------------------------------------------------------------------------

func TestMessSchemaAlwaysWritten(t *testing.T) {
	cases := []struct {
		name string
		tool string
	}{
		{"missing", `{"type":"function","function":{"name":"f","description":"d"}}`},
		{"null", `{"type":"function","function":{"name":"f","description":"d","parameters":null}}`},
		{"string", `{"type":"function","function":{"name":"f","description":"d","parameters":"{}"}}`},
		{"array", `{"type":"function","function":{"name":"f","description":"d","parameters":[]}}`},
		{"number", `{"type":"function","function":{"name":"f","description":"d","parameters":123}}`},
		{"empty_object", `{"type":"function","function":{"name":"f","description":"d","parameters":{}}}`},
		{"no_name", `{"type":"function","function":{"description":"d"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := appendToolsMapped(nil, gjson.Parse("["+c.tool+"]"), toolsMessFromChat)
			require.True(t, strings.HasPrefix(string(out), "{"))
			obj := gjson.Parse("[" + string(out) + "]").Array()[0]
			// out-1：input_schema 必须存在
			schema := obj.Get("input_schema")
			require.True(t, schema.Exists(), "%s: 必须写出 input_schema（%s）", c.name, out)
			assertPOut(t, c.name, schema.Raw)
			// 无参工具的 description 不得被追加"约束被简化"提示（I-4）
			if c.name != "missing" && c.name != "null" && c.name != "string" && c.name != "array" && c.name != "number" {
				return
			}
			desc := obj.Get("description").Str
			require.Equal(t, "d", desc, "%s: 第 0 步不是退化，不得追加 hint", c.name)
		})
	}
}

// (d) 的 required 交集断言：两分支 required 不同 ⇒ 交集为空 ⇒ 产物不得有 required。
func TestReportedRequiredIntersection(t *testing.T) {
	src := `{"type":"object","$defs":{"A":{"type":"object","properties":{"p":{"type":"string"}},"required":["p"]},"B":{"type":"object","properties":{"r":{"type":"number"}},"required":["r"]}},"oneOf":[{"$ref":"#/$defs/A"},{"$ref":"#/$defs/B"}]}`
	out, deg := messSchema(src)
	require.NotEqual(t, degBail, deg)
	require.False(t, gjson.Parse(out).Get("required").Exists(),
		"oneOf 的 required 必须取交（两分支不同 ⇒ 空集），产物=%s", out)
	require.True(t, gjson.Parse(out).Get("properties.p").Exists())
	require.True(t, gjson.Parse(out).Get("properties.r").Exists())
}

// ---------------------------------------------------------------------------
// 模式零值必须 panic（spec §3.5：不能只靠 init 自检）
// ---------------------------------------------------------------------------

func TestUnknownSchemaModePanics(t *testing.T) {
	require.Panics(t, func() {
		evalSchemaField(toolsFieldMap{mode: schemaModeUnknown}, gjson.Parse(`{"name":"f"}`))
	})
}

// ---------------------------------------------------------------------------
// (n) 日志与度量
// ---------------------------------------------------------------------------

// (n)① 白名单 ⊆ 实测：每个 reason 至少被一行用例触发（反向断言见 TestReasonWhitelist）。
func TestReasonCoverage(t *testing.T) {
	used := map[string]struct{}{}
	for _, r := range loadSchemaCases(t) {
		for _, got := range evalMessSchema(gjson.Parse(r.src)).reasons {
			used[got] = struct{}{}
		}
	}
	for _, want := range reasonOrder {
		_, ok := used[want]
		require.True(t, ok, "(n)① reason %q 没有任何用例触发（白名单里有、实现里不发）", want)
	}
}

// (n)③④⑤⑥ 观测事件与计数。
func TestSchemaObservability(t *testing.T) {
	resetSchemaRepairedCounts()
	var events []SchemaEvent
	SetSchemaSink(func(e SchemaEvent) { events = append(events, e) })
	defer SetSchemaSink(nil)

	tools := gjson.Parse(`[
		{"type":"function","function":{"name":"lossy","description":"d","parameters":{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}]}}},
		{"type":"function","function":{"name":"noparam","description":"d"}},
		{"type":"function","function":{"description":"d","parameters":{"type":"object","oneOf":[{"type":"object","$defs":{"P":{"type":"object"}},"properties":{"a":{"$ref":"#/$defs/P"}}}]}}}
	]`)
	out := appendToolsMapped(nil, tools, toolsMessFromChat)
	require.Len(t, events, 3, "每个 mess 方言工具都必须产生一条事件：%s", out)

	byTool := map[string]SchemaEvent{}
	for _, e := range events {
		byTool[e.Tool] = e
		for _, r := range e.Reasons {
			_, ok := reasonWhitelist[r]
			require.True(t, ok, "(n)① 未在白名单的 reason：%q", r)
		}
	}
	// ① 简化退化：WARN 路径带 root_keys，且 description 追加一次 hint
	lossy := byTool["lossy"]
	require.Equal(t, "simplify", lossy.Degenerate)
	require.Equal(t, "chat_to_mess", lossy.Direction)
	require.Greater(t, lossy.RootKeys, 0, "WARN 路径必须记录 root_keys")
	require.Contains(t, lossy.Reasons, "degenerate-lossy")
	require.Equal(t, 1, strings.Count(gjson.Parse("[" + string(out) + "]").Array()[0].Get("description").Str, "args simplified"),
		"hint 只出现一次")

	// ② 无参工具：missing=true、reason 含 no-schema、不退化、不加 hint
	np := byTool["noparam"]
	require.True(t, np.Missing)
	require.Equal(t, "none", np.Degenerate)
	require.Equal(t, 0, np.SchemaBytes)
	require.Contains(t, np.Reasons, "no-schema")
	require.Equal(t, "d", gjson.Parse("[" + string(out) + "]").Array()[1].Get("description").Str)

	// ③ 源工具无 name ⇒ tool==""（(n)③ 的正例）
	anon := byTool[""]
	require.Equal(t, "bail", anon.Degenerate)
	require.Contains(t, anon.Reasons, "dangling-after-flatten")

	// ⑥ 兜底每次计 1
	require.EqualValues(t, 1, SchemaRepaired("dangling-after-flatten"))
	require.Greater(t, SchemaRepaired("no-schema"), int64(0))
	counts := SchemaRepairedCounts()
	require.NotEmpty(t, counts)
}

// (n)⑤ 两个 reason 各自可触发：dangling-after-flatten 与 unresolved-ref 各被至少一行触发。
func TestReasonTriggers(t *testing.T) {
	seen := map[string]int{}
	for _, r := range loadSchemaCases(t) {
		for _, got := range evalMessSchema(gjson.Parse(r.src)).reasons {
			seen[got]++
		}
	}
	require.Greater(t, seen["dangling-after-flatten"], 0, "9b 兜底必须被至少一行触发")
	require.Greater(t, seen["unresolved-ref"], 0, "源侧读不到必须被至少一行触发")
	require.Greater(t, seen["merged-union"], 0)
	require.Greater(t, seen["duplicate-key"], 0)
	// duplicate-key 不得由 $ref 伪造：单独核对一行"只有 $ref、无重复键"的源
	res := evalMessSchema(gjson.Parse(`{"type":"object","$ref":"#/$defs/P","$defs":{"P":{"type":"object"}}}`))
	require.NotContains(t, res.reasons, "duplicate-key", "无重复键的源不得出现 duplicate-key（I-2）")
}

// ---------------------------------------------------------------------------
// (g) 基准门禁：数值断言 + 源 sha256 棘轮
// ---------------------------------------------------------------------------

type benchTarget struct {
	allocs int
	ns     int64
}

// fileSHA256 计算源文件 sha256（M0 棘轮用）。
func fileSHA256(name string) (string, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func loadBenchTargets(t *testing.T) map[string]benchTarget {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "spec", "bench_targets.tsv"))
	require.NoError(t, err)
	out := map[string]benchTarget{}
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || line == "\r" {
			continue
		}
		f := splitTSVLine(line)
		if i == 0 && f[0] == "benchmark" {
			continue
		}
		require.Len(t, f, 4, "bench_targets.tsv 第 %d 行必须 4 列", i+1)
		allocs, err := strconv.Atoi(f[1])
		require.NoError(t, err)
		ns, err := strconv.Atoi(f[2])
		require.NoError(t, err)
		out[f[0]] = benchTarget{allocs: allocs, ns: int64(ns)}
	}
	require.NotEmpty(t, out)
	return out
}

// bestOf 跑 n 次 bench 取最优（并发测试环境下单次读数受调度噪声影响大）。
func bestOf(n int, f func() testing.BenchmarkResult) testing.BenchmarkResult {
	best := f()
	for i := 1; i < n; i++ {
		cur := f()
		if cur.NsPerOp() < best.NsPerOp() {
			best = cur
		}
	}
	return best
}

// TestBenchGates 真跑 bench 并与 M 记录表比对（(g)：数值断言，不是文件子串）。
//
// 必须**单独跑**：ns/op 断言只在空闲机器上有意义，与其余包并行（scripts/test.sh
// 的 -p）一起跑时读到过 2.7×（同一二进制单跑 0.9–1.0×）。脚本因此跳过它、再串行
// 补跑（`-run '^TestBenchGates$' ./internal/protoconv`）——以后新增 timing 断言
// 沿用同一条纪律，别把 ns 比较塞进并行那一遍。
func TestBenchGates(t *testing.T) {
	targets := loadBenchTargets(t)
	// spec §8(o)②：两个新方向的 allocs 上界**写死在代码里**（首测值 ×1.2 取整），
	// 免得"改 TSV 就顺手放宽"。改上界必须同时改这里与 bench_targets.tsv。
	for name, ceil := range map[string]int{
		"BenchmarkConvertRequestMessToChatTools20": 31, // 26 allocs ×1.2
		"BenchmarkConvertRequestRespToChatTools20": 28, // 24 allocs ×1.2
	} {
		tgt, ok := targets[name]
		require.True(t, ok, "bench_targets.tsv 缺 %s", name)
		require.LessOrEqual(t, tgt.allocs, ceil, "%s 的登记 allocs 超过首测值×1.2 的上界", name)
		require.NotZero(t, tgt.ns, "%s 的 ns 必须登记（0 会让相对门槛恒红）", name)
	}

	// Scan 必须 0 分配（spec §4 决策 1/3/6），且测的是生产热路径 pInFast。
	scan := bestOf(3, func() testing.BenchmarkResult { return testing.Benchmark(BenchmarkMessSchemaScan) })
	require.EqualValues(t, 0, scan.AllocsPerOp(), "BenchmarkMessSchemaScan 必须 0 分配（pInFast 的固定数组）")
	require.LessOrEqual(t, scan.AllocsPerOp(), int64(targets["BenchmarkMessSchemaScan"].allocs))
	if !raceEnabled {
		// 亚微秒基准的相对噪声远大于微秒级基准 ⇒ 这里给 3x（其余 2.5x，见 spec L25）。
		require.LessOrEqual(t, scan.NsPerOp(), targets["BenchmarkMessSchemaScan"].ns*3,
			"BenchmarkMessSchemaScan 的 ns/op 相对 M 记录超限（%d ns）", scan.NsPerOp())
	}

	check := func(name string, r testing.BenchmarkResult) {
		tgt, ok := targets[name]
		require.True(t, ok, "bench_targets.tsv 缺 %s", name)
		require.LessOrEqual(t, r.AllocsPerOp(), int64(tgt.allocs), "%s 的 allocs/op 不得比 M 记录差", name)
		if raceEnabled {
			return // -race 下 ns 放大数倍，不可比（L25）；allocs 仍然有效
		}
		// 相对阈值：同机允许抖动与并发干扰（3 次取最优），但数量级变差必须红。
		require.LessOrEqual(t, r.NsPerOp(), tgt.ns*5/2, "%s 的 ns/op 相对 M 记录超限（%d ns）", name, r.NsPerOp())
	}
	if !testing.Short() {
		check("BenchmarkMessSchemaFlatten", bestOf(3, func() testing.BenchmarkResult { return testing.Benchmark(BenchmarkMessSchemaFlatten) }))
		check("BenchmarkConvertRequestRespToMessTools20", bestOf(3, func() testing.BenchmarkResult {
			return testing.Benchmark(BenchmarkConvertRequestRespToMessTools20)
		}))
		// spec §8(o)①：两个新方向的 benchmark 必须真的被门禁跑到，否则
		// bench_targets.tsv 里那两行是死数据。
		check("BenchmarkConvertRequestMessToChatTools20", bestOf(3, func() testing.BenchmarkResult {
			return testing.Benchmark(BenchmarkConvertRequestMessToChatTools20)
		}))
		check("BenchmarkConvertRequestRespToChatTools20", bestOf(3, func() testing.BenchmarkResult {
			return testing.Benchmark(BenchmarkConvertRequestRespToChatTools20)
		}))
	}
}

// M0 记录必须与当前源文件逐字节一致（否则 bench 数字已失效）。
func TestBenchRecordMatchesSource(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "spec", "protoconv-toolschema-bench-M0.txt"))
	require.NoError(t, err)
	txt := string(raw)
	for _, must := range []string{"goos=", "goarch=", "cpu=", "pkg=", "-count=", "src_sha256 "} {
		require.Contains(t, txt, must, "M0 记录缺 %s", must)
	}
	for file, field := range map[string]string{
		"toolschema.go":            "src_sha256 toolschema.go",
		"toolschema_bench_test.go": "src_sha256 toolschema_bench_test.go",
	} {
		want := ""
		for _, line := range strings.Split(txt, "\n") {
			if strings.HasPrefix(line, field+" ") {
				want = strings.TrimSpace(strings.TrimPrefix(line, field))
			}
		}
		require.NotEmpty(t, want, "M0 记录缺 %s 的 sha256", file)
		sum, err := fileSHA256(file)
		require.NoError(t, err)
		require.Equal(t, want, sum, "%s 的 sha256 与 M0 记录不一致（bench 数字已失效，需重跑并更新 M0）", file)
	}
}

// ---------------------------------------------------------------------------
// metis-r2 的三条回归（互指 $ref 的递归、畸形 JSON 的快路径、工具级重复键）
// ---------------------------------------------------------------------------

// C-1：4b 的递归必须有保险丝（互指的 $ref + 组合关键字没有自然终点）。
func TestFlattenRecursionBounded(t *testing.T) {
	cases := []string{
		`{"type":"object","$ref":"#/$defs/A","$defs":{"A":{"oneOf":[{"$ref":"#/$defs/A"}]}}}`,
		`{"type":"object","oneOf":[{"$ref":"#/$defs/A"}],"$defs":{"A":{"anyOf":[{"$ref":"#/$defs/A"}]}}}`,
	}
	for _, src := range cases {
		out, deg := messSchema(src)
		require.True(t, gjson.Valid(out), "产物必须是合法 JSON：%s", out)
		assertPOut(t, "cycle", out)
		require.Equal(t, degSimplify, deg, "深度保险丝触发必须计 lossy（发 hint）")
	}
}

// C-2：畸形 schema **绝不允许原样透传**。
// 口径（spec L32）：请求体入口只做括号/引号配对检查（与既有宽容行为一致，
// 不因畸形 body 整条拒绝）；而"原样透传源字节"这条路上必须严格校验，
// 否则 {"type":"object",} 会被写进 input_schema。改写路径的产物是重新拼的，不受影响。
func TestMalformedSchemaNeverPassedThrough(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object",}}}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object" "properties":{}}}}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":}}}]}`,
	}
	for _, b := range bodies {
		out, err := ConvertRequest([]byte(b), domain.ProtocolConvertChatToMess)
		require.NoError(t, err, "畸形 body 不整条拒绝（与既有宽容行为一致）：%s", b)
		schema := gjson.Parse(string(out)).Get("tools.0.input_schema")
		require.True(t, gjson.Valid(schema.Raw), "input_schema 必须是合法 JSON：%s ⇒ %s", b, schema.Raw)
		require.Equal(t, `{"type":"object"}`, schema.Raw, "畸形 schema 必须兜底成空 schema：%s", b)
	}
}

// C-3：工具对象里的重复键必须末个胜（gjson.Get 是首胜 ⇒ 收窄）。
func TestToolDuplicateKeyLastWins(t *testing.T) {
	tools := gjson.Parse(`[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},"parameters":{"type":"object"}}}]`)
	out := string(appendToolsMapped(nil, tools, toolsMessFromChat))
	require.Equal(t, `{"input_schema":{"type":"object"},"name":"f"}`, out,
		"重复 parameters 必须取末个（上游 json.loads 也是末个胜）")

	// 重复 name / description 同理
	tools = gjson.Parse(`[{"type":"function","function":{"name":"first","name":"last","description":"d1","description":"d2","parameters":{"type":"object"}}}]`)
	out = string(appendToolsMapped(nil, tools, toolsMessFromChat))
	require.Equal(t, `{"description":"d2","input_schema":{"type":"object"},"name":"last"}`, out)
}

// C-1（metis-r3）：每层两个 $ref 指回下一层 ⇒ 调用数按 2^n 爆炸；预算必须挡住。
func TestFlattenBudgetBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"object","oneOf":[{"$ref":"#/$defs/D0"}],"$defs":{`)
	const levels = 20
	for i := 0; i < levels; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		next := i + 1
		if i == levels-1 {
			next = i // 末端自指，保证递归无自然终点
		}
		fmt.Fprintf(&sb, `"D%d":{"oneOf":[{"$ref":"#/$defs/D%d"},{"$ref":"#/$defs/D%d"}]}`, i, next, next)
	}
	sb.WriteString(`}}`)
	start := time.Now()
	out, deg := messSchema(sb.String())
	elapsed := time.Since(start)
	require.Less(t, elapsed, 2*time.Second, "指数爆炸没被预算挡住：%v", elapsed)
	require.True(t, gjson.Valid(out), "产物必须是合法 JSON")
	require.Equal(t, degSimplify, deg, "预算耗尽必须计 lossy（发 hint）")
}

// I-1（metis-r3）：环 / 非对象目标记 unresolved-ref，但没有内容可丢 ⇒ 不 lossy。
func TestCycleReasonNotLossy(t *testing.T) {
	cases := []struct{ name, src string }{
		{"self_cycle", `{"type":"object","$ref":"#/$defs/A","$defs":{"A":{"$ref":"#/$defs/B"},"B":{"$ref":"#/$defs/A"}}}`},
		{"non_object_target", `{"type":"object","oneOf":[{"$ref":"#/$defs/S"}],"$defs":{"S":"not-a-schema"}}`},
	}
	for _, c := range cases {
		res := evalMessSchema(gjson.Parse(c.src))
		require.Contains(t, res.reasons, "unresolved-ref", "%s: 必须记 unresolved-ref", c.name)
		require.NotContains(t, res.reasons, "degenerate-lossy", "%s: 环/非对象目标不计 lossy", c.name)
	}
}
