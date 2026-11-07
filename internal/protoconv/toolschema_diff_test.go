package protoconv

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// (i) 差分回归：rawKeyEq（[]byte 版）必须与 gjsonKeyEq 逐项一致。
func TestRawKeyEqMatchesGjson(t *testing.T) {
	cases := []struct{ key, name string }{
		{`"a"`, "a"},
		{`"abc"`, "abc"},
		{`"abc"`, "ab"},
		{`"abc"`, "abcd"},
		{`"\u0061"`, "a"},
		{`"a\u0062c"`, "abc"},
		{`"A"`, "a"},
		{`"a b"`, "a b"},
		{`"a\nb"`, "a\nb"},
		{`"a\"b"`, `a"b`},
		{`"\\"`, `\`},
		{`"日本"`, "日本"},
	}
	for _, c := range cases {
		got := rawKeyEq([]byte(c.key), c.name)
		want := gjsonKeyEq(gjson.Parse(c.key), c.name)
		require.Equal(t, want, got, "rawKeyEq(%s, %q)", c.key, c.name)
	}
}

// (n)① 反向断言：实现里出现的 reason 字面量必须都在白名单内；
// 正向断言（白名单 ⊆ 实测）在 TestReasonCoverage。本测试不再"自己查自己"。
func TestReasonWhitelist(t *testing.T) {
	raw, err := os.ReadFile("toolschema.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`st\.reason\("([a-z-]+)"\)`)
	found := 0
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		found++
		_, ok := reasonWhitelist[m[1]]
		require.True(t, ok, "reason %q 不在白名单（(n)① 反向断言）", m[1])
	}
	require.Greater(t, found, 3, "至少要扫到几处 reason 调用，否则正则失效")
	// 白名单自身必须无重复、且与 reasonOrder 完全一致（顺序表是唯一真源）。
	require.Len(t, reasonWhitelist, len(reasonOrder), "reasonOrder 与 reasonWhitelist 必须一一对应")
	for _, r := range reasonOrder {
		_, ok := reasonWhitelist[r]
		require.True(t, ok, "reasonOrder 里的 %q 不在白名单", r)
	}
}

// (n)④ hint 幂等：同一份提示只追加一次；两份提示互不判重。
func TestHintIdempotent(t *testing.T) {
	base := "do the thing"
	once := string(encodeDescriptionWithHint(base, degBail))
	twice := string(encodeDescriptionWithHint(gjson.Parse(once).Str, degBail))
	require.Equal(t, once, twice, "兜底提示必须幂等")

	// 常规提示与兜底提示是两份文案，互不判重
	other := string(encodeDescriptionWithHint(gjson.Parse(once).Str, degSimplify))
	require.Contains(t, other, "simplified")

	// 超长不追加
	huge := ""
	for i := 0; i < maxDescriptionBytes+10; i++ {
		huge += "x"
	}
	require.Equal(t, `"`+huge+`"`, string(encodeDescriptionWithHint(huge, degBail)))
}
