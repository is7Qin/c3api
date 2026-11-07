package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// messOne 单个 Mess 工具（源）。
func messOne(schemaJSON string) gjson.Result {
	return gjson.Parse(`[{"name":"f","description":"d","input_schema":` + schemaJSON + `}]`)
}

// (o) mess→resp 七格逐字节：源 input_schema 非对象/缺失 ⇒ 省略 parameters。
func TestFreezeMessToResp(t *testing.T) {
	const want = `{"description":"d","name":"f","type":"function"}`
	for _, src := range []string{`"str"`, `[]`, `123`, `true`, `null`, ``, `"{}"`} {
		var tools gjson.Result
		if src == "" {
			tools = gjson.Parse(`[{"name":"f","description":"d"}]`)
		} else {
			tools = messOne(src)
		}
		got := string(appendToolsMapped(nil, tools, toolsRespFromMess))
		require.Equal(t, want, got, "源 input_schema=%s 必须省略 parameters", src)
	}
	// 对象形态 ⇒ 含 parameters（6 格：{}、{"type":"object"}）
	for _, src := range []string{`{}`, `{"type":"object"}`} {
		got := string(appendToolsMapped(nil, messOne(src), toolsRespFromMess))
		require.Equal(t, `{"description":"d","name":"f","parameters":`+src+`,"type":"function"}`, got)
	}
}

// (o2) chat→resp 十一格逐字节：该映射是 Existence 语义，与 (o) 有三格必然相反。
func TestFreezeChatToResp(t *testing.T) {
	chat := func(params string) gjson.Result {
		return gjson.Parse(`[{"type":"function","function":{"name":"f","description":"d"` + params + `}}]`)
	}
	cases := []struct{ name, params, want string }{
		{"str", `,"parameters":"{}"`, `{"description":"d","name":"f","parameters":"{}","type":"function"}`},
		{"arr", `,"parameters":[]`, `{"description":"d","name":"f","parameters":[],"type":"function"}`},
		{"num", `,"parameters":123`, `{"description":"d","name":"f","parameters":123,"type":"function"}`},
		{"null", `,"parameters":null`, `{"description":"d","name":"f","type":"function"}`},
		{"missing", ``, `{"description":"d","name":"f","type":"function"}`},
		{"empty", `,"parameters":{}`, `{"description":"d","name":"f","parameters":{},"type":"function"}`},
		{"object", `,"parameters":{"type":"object"}`, `{"description":"d","name":"f","parameters":{"type":"object"},"type":"function"}`},
		{"union", `,"parameters":{"oneOf":[{"type":"object"}]}`, `{"description":"d","name":"f","parameters":{"oneOf":[{"type":"object"}]},"type":"function"}`},
		{"union_strict", `,"parameters":{"oneOf":[{"type":"object"}]},"strict":true`, `{"description":"d","name":"f","parameters":{"oneOf":[{"type":"object"}]},"strict":true,"type":"function"}`},
		{"strict_true_only", `,"strict":true`, `{"description":"d","name":"f","strict":true,"type":"function"}`},
		{"strict_non_bool", `,"strict":"true"`, `{"description":"d","name":"f","type":"function"}`},
	}
	require.Len(t, cases, 11)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(appendToolsMapped(nil, chat(c.params), toolsRespFromChat))
			require.Equal(t, c.want, got)
		})
	}
}

// (d) 上报报文回归：$defs(25) + oneOf(4×$ref) 的旗舰形态。
func TestReportedPacket(t *testing.T) {
	var defs, branches string
	for i := 0; i < 25; i++ {
		if i > 0 {
			defs += ","
		}
		defs += `"D` + itoa(i) + `":{"type":"object","properties":{"p` + itoa(i) + `":{"type":"string"}}}`
	}
	for i := 0; i < 4; i++ {
		if i > 0 {
			branches += ","
		}
		branches += `{"$ref":"#/$defs/D` + itoa(i) + `"}`
	}
	src := `{"type":"object","$defs":{` + defs + `},"oneOf":[` + branches + `]}`
	out, deg := messSchema(src)
	require.Equal(t, degNone, deg)
	assertPOut(t, "reported_packet", out)
	obj := gjson.Parse(out)
	require.Len(t, obj.Get("$defs").Map(), 25, "$defs 必须保留 25 条")
	// 四分支的 p0..p3 必须在 properties 里
	for i := 0; i < 4; i++ {
		require.True(t, obj.Get("properties.p"+itoa(i)).Exists(), "缺 p%d", i)
	}
	// 引用必须仍在且可解析
	require.True(t, valveOK([]byte(out)))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
