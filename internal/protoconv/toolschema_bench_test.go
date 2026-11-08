package protoconv

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/tidwall/gjson"
)

// toolsSchema20 生成 spec §4 行 B 的载荷：19 个普通工具 + 1 个带
// $defs(25) + oneOf(4×$ref) 的工具；schema 约 10 KB、body 约 12 KB。
func toolsSchema20() string {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[`)
	for i := 0; i < 19; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"function","name":"t%d","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}}`, i)
	}
	sb.WriteString(`,`)
	sb.WriteString(`{"type":"function","name":"automation_update","description":"d","parameters":{"type":"object","$defs":{`)
	for i := 0; i < 25; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"D%d":{"type":"object","properties":{"p%d":{"type":"string"},"q%d":{"type":"boolean"}}}`, i, i, i)
	}
	sb.WriteString(`},"oneOf":[`)
	for i := 0; i < 4; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"$ref":"#/$defs/D%d"}`, i)
	}
	sb.WriteString(`]}}]}`)
	return sb.String()
}

var benchTools20 = toolsSchema20()

var benchTools20Result []byte

// BenchmarkConvertRequestRespToMessTools20 全量转换（20 工具）。
func BenchmarkConvertRequestRespToMessTools20(b *testing.B) {
	body := []byte(benchTools20)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertRequest(body, domain.ProtocolConvertRespToMess)
		if err != nil {
			b.Fatal(err)
		}
		benchTools20Result = out
	}
}

// toolsSchema20AllPIn 20 个工具**全部**满足 P_in（无根级组合关键字）⇒
// 每个工具都走短路（锁"不该改的一个都不多花"）。
func toolsSchema20AllPIn() string {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"function","name":"t%d","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}}`, i)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

var benchTools20Idle = toolsSchema20AllPIn()

// BenchmarkConvertRequestRespToMessTools20AllPIn 全命中场景（真实流量的大多数）。
func BenchmarkConvertRequestRespToMessTools20AllPIn(b *testing.B) {
	body := []byte(benchTools20Idle)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertRequest(body, domain.ProtocolConvertRespToMess)
		if err != nil {
			b.Fatal(err)
		}
		benchTools20Result = out
	}
}

// benchMessToChat20 mess → chat 的 20 工具请求体（工具数是真实流量的上限形态）。
func benchMessToChat20() string {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","max_tokens":4096,"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]},{"role":"assistant","content":[{"type":"text","text":"t"},{"type":"tool_use","id":"a","name":"t0","input":{"a":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"ok"}]}]}],"tools":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"name":"t%d","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}}`, i)
	}
	sb.WriteString(`],"tool_choice":{"type":"auto"}}`)
	return sb.String()
}

// benchRespToChat20 resp → chat 的 20 工具请求体。
func benchRespToChat20() string {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","instructions":"sys","max_output_tokens":4096,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://e.com/i.png"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"type":"function_call","call_id":"a","name":"t0","arguments":"{\"a\":\"x\"}"},{"type":"function_call_output","call_id":"a","output":"ok"}],"tools":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"function","name":"t%d","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}}`, i)
	}
	sb.WriteString(`],"tool_choice":{"type":"function","name":"t0"}}`)
	return sb.String()
}

var (
	benchMessToChat20Body = benchMessToChat20()
	benchRespToChat20Body = benchRespToChat20()
)

// BenchmarkConvertRequestMessToChatTools20 全量转换（T8，20 工具）。
func BenchmarkConvertRequestMessToChatTools20(b *testing.B) {
	body := []byte(benchMessToChat20Body)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertRequest(body, domain.ProtocolConvertMessToChat)
		if err != nil {
			b.Fatal(err)
		}
		benchTools20Result = out
	}
}

// BenchmarkConvertRequestRespToChatTools20 全量转换（T10，20 工具）。
func BenchmarkConvertRequestRespToChatTools20(b *testing.B) {
	body := []byte(benchRespToChat20Body)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertRequest(body, domain.ProtocolConvertRespToChat)
		if err != nil {
			b.Fatal(err)
		}
		benchTools20Result = out
	}
}

// BenchmarkToolsHandling 只做"现状工具字节处理"（ForEach + Get + append(Raw)），
// 作为分母，不是门禁。
func BenchmarkToolsHandling(b *testing.B) {
	tools := gjson.Parse(benchTools20).Get("tools")
	var out []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out = out[:0]
		tools.ForEach(func(_, tv gjson.Result) bool {
			out = append(out, tv.Get("parameters").Raw...)
			return true
		})
	}
	benchTools20Result = out
}

// BenchmarkMessSchemaScan 生产热路径：P_in 快路径（pInFast）的根级一趟扫描，
// 0 分配。scanRoot 是 !pInFast 之后的慢路径，不在本基准里（spec v21 登记）。
func BenchmarkMessSchemaScan(b *testing.B) {
	src := gjson.Parse(benchTools20).Get("tools.0.parameters")
	b.SetBytes(int64(len(src.Raw)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !pInFast(src) {
			b.Fatal("pInFast 应命中：载荷是合法 object schema")
		}
	}
}

// BenchmarkMessSchemaFlatten 压平（单 union schema）。
func BenchmarkMessSchemaFlatten(b *testing.B) {
	src := gjson.Parse(`{"type":"object","$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}}}},"oneOf":[{"$ref":"#/$defs/P"}]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchTools20Result = evalMessSchema(src).out
	}
}

// BenchmarkToolsRatio 同进程比值：一个 b.N 循环里先跑工具字节处理、再跑修复路径，
// 输出比值（只记账，不断言）。
func BenchmarkToolsRatio(b *testing.B) {
	tools := gjson.Parse(benchTools20).Get("tools")
	src := gjson.Parse(`{"type":"object","$defs":{"P":{"type":"object","properties":{"q":{"type":"string"}}}},"oneOf":[{"$ref":"#/$defs/P"}]}`)
	var out []byte
	var base, fix time.Duration
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		out = out[:0]
		tools.ForEach(func(_, tv gjson.Result) bool {
			out = append(out, tv.Get("parameters").Raw...)
			return true
		})
		d0 := time.Since(t0)
		t1 := time.Now()
		out = append(out, evalMessSchema(src).out...)
		d1 := time.Since(t1)
		base += d0
		fix += d1
	}
	benchTools20Result = out
	if base > 0 {
		b.ReportMetric(float64(fix)/float64(base), "fix/base")
	}
}
