// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件钉住 spec 2026-11-16 §11-T1（校验 V1–V9）：
//   - 每个拒绝类一条子用例（名字带 V 编号，便于对 spec 表格复盘）；
//   - 边界值必须通过（32 字节 id、40 rune label、2048 字节 url、大写 scheme、query+fragment）；
//   - 空值语义、enabled 缺省、未知字段、归一化幂等、序列化字节与键序。
//
// 断言只用 require（失败即停，后续断言在已坏的前提下没有意义）；不用 t.Parallel：
// 这些是纯函数，并行只增加输出噪声，不换覆盖率。

// customPageFixture 构造一条合法条目。拒绝类用例只改坏需要破坏的那个字段，
// 这样失败信息指向真正的规则，而不是「六个字段里有一个拼错了」。
func customPageFixture() CustomPage {
	return CustomPage{
		ID:         "docs",
		Label:      "文档",
		URL:        "https://example.com/docs",
		Visibility: CustomPageVisibilityUser,
		Icon:       CustomPageIconBook,
		Enabled:    true,
	}
}

// customPageWith 返回只含一条的列表，mutate 就地改坏某个字段。
func customPageWith(mutate func(*CustomPage)) []CustomPage {
	page := customPageFixture()
	mutate(&page)
	return []CustomPage{page}
}

// customPageList 生成 n 条 id 唯一的合法条目（id 形如 page-00）。
func customPageList(n int) []CustomPage {
	pages := make([]CustomPage, 0, n)
	for i := 0; i < n; i++ {
		page := customPageFixture()
		page.ID = fmt.Sprintf("page-%02d", i)
		pages = append(pages, page)
	}
	return pages
}

// TestCustomPageVisibilityValid V7 的值域本体：只有 admin/user 合法，
// 空串与任意拼写都不是「缺省」——缺省补齐是 Normalize 的事。
func TestCustomPageVisibilityValid(t *testing.T) {
	require.True(t, CustomPageVisibilityAdmin.Valid())
	require.True(t, CustomPageVisibilityUser.Valid())
	require.False(t, CustomPageVisibility("").Valid())
	require.False(t, CustomPageVisibility("all").Valid())
	require.False(t, CustomPageVisibility("Admin").Valid(), "枚举大小写敏感")
}

// TestCustomPageIconValid V8 的值域本体 + CustomPageIcons 顺序（§4.2：顺序即
// 前端下拉顺序，T4 门禁会与 TS 逐位比对；这里先钉住本仓内的字面量）。
func TestCustomPageIconValid(t *testing.T) {
	require.Equal(t,
		[]string{"link", "globe", "book", "chart", "lifebuoy", "ticket", "code", "sparkles"},
		CustomPageIcons)
	for _, icon := range CustomPageIcons {
		require.True(t, CustomPageIcon(icon).Valid(), icon)
	}
	require.False(t, CustomPageIcon("").Valid())
	require.False(t, CustomPageIcon("rocket").Valid())
}

// TestCustomPageIsVisibleTo 可见性判定是「启用 && 角色匹配」，停用的条目两侧
// 都不可见（§4.4 的过滤只调用这两个方法）。
func TestCustomPageIsVisibleTo(t *testing.T) {
	tests := []struct {
		name     string
		page     CustomPage
		wantAdm  bool
		wantUser bool
	}{
		{"admin_enabled", CustomPage{Visibility: CustomPageVisibilityAdmin, Enabled: true}, true, false},
		{"user_enabled", CustomPage{Visibility: CustomPageVisibilityUser, Enabled: true}, false, true},
		{"admin_disabled", CustomPage{Visibility: CustomPageVisibilityAdmin, Enabled: false}, false, false},
		{"user_disabled", CustomPage{Visibility: CustomPageVisibilityUser, Enabled: false}, false, false},
		{"invalid_visibility", CustomPage{Visibility: CustomPageVisibility("all"), Enabled: true}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantAdm, tt.page.IsVisibleToAdmin())
			require.Equal(t, tt.wantUser, tt.page.IsVisibleToUser())
		})
	}
}

// TestParseCustomPagesEmptyForms 空值语义（§4.3 D11）：""/"[]"/"null" 都是
// 「未配置」，必须不报错；手改 DB 时的空白填充也一并容忍。
func TestParseCustomPagesEmptyForms(t *testing.T) {
	for _, raw := range []string{"", "[]", "null", "  ", " [ ] ", "null\n"} {
		pages, err := ParseCustomPages(raw)
		require.NoError(t, err, "raw=%q", raw)
		require.Empty(t, pages, "raw=%q", raw)
	}
}

// TestParseCustomPagesDefaultsAndUnknownFields enabled 缺省 = true、显式 false
// 保留、未知字段忽略（前向兼容）；parse 本身不 trim、不补缺省。
func TestParseCustomPagesDefaultsAndUnknownFields(t *testing.T) {
	raw := `[{"id":"docs","label":"  Docs  ","url":"https://example.com/docs","visibility":"admin","icon":"book","unknown_field":{"nested":true}},` +
		`{"id":"blog","label":"Blog","url":"https://blog.example.com","enabled":false}]`
	pages, err := ParseCustomPages(raw)
	require.NoError(t, err)
	require.Len(t, pages, 2)

	require.Equal(t, "docs", pages[0].ID)
	require.Equal(t, CustomPageVisibilityAdmin, pages[0].Visibility)
	require.Equal(t, CustomPageIconBook, pages[0].Icon)
	require.True(t, pages[0].Enabled, "enabled 缺省必须是 true（D11：少写字段页面不能消失）")
	require.Equal(t, "  Docs  ", pages[0].Label, "parse 不 trim，归一是 Normalize 的职责")

	require.False(t, pages[1].Enabled, "显式 false 必须保留")
	require.Empty(t, pages[1].Visibility, "parse 不补缺省")
	require.Empty(t, pages[1].Icon)
}

// TestParseCustomPagesRejects V1：非空 raw 必须是 JSON 数组；enabled 必须是
// JSON 布尔（"true"/1 由 encoding/json 直接拒，§4.2）。
func TestParseCustomPagesRejects(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"V1_object", `{"id":"docs"}`, "custom pages"},
		{"V1_string", `"[]"`, "custom pages"},
		{"V1_number", `123`, "custom pages"},
		{"enabled_string", `[{"id":"docs","enabled":"true"}]`, "custom pages"},
		{"enabled_number", `[{"id":"docs","enabled":1}]`, "custom pages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pages, err := ParseCustomPages(tt.raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Nil(t, pages)
		})
	}
}

// TestValidateCustomPagesAccepts 边界值必须通过：上限本身是合法的（拒绝的是
// 上限 + 1），大写 scheme 与 query/fragment 都属于正常配置。
func TestValidateCustomPagesAccepts(t *testing.T) {
	maxURL := "https://example.com/" + strings.Repeat("a", CustomPageMaxURLBytes-len("https://example.com/"))
	tests := []struct {
		name  string
		pages []CustomPage
	}{
		{"empty_nil", nil},
		{"empty_slice", []CustomPage{}},
		{"minimal", []CustomPage{customPageFixture()}},
		{"V2_max_items", customPageList(CustomPageMaxItems)},
		{"V3_id_32_bytes", customPageWith(func(p *CustomPage) { p.ID = strings.Repeat("a", CustomPageMaxIDBytes) })},
		{"V3_id_digits_and_dashes", customPageWith(func(p *CustomPage) { p.ID = "9-a-0" })},
		{"V5_label_40_runes", customPageWith(func(p *CustomPage) { p.Label = strings.Repeat("汉", CustomPageMaxLabelRunes) })},
		{"V5_label_padded_to_40_runes", customPageWith(func(p *CustomPage) {
			// trim 后正好 40 rune：V5 判的是 trim 后的长度，两侧空白由 Normalize 去掉。
			p.Label = "  " + strings.Repeat("汉", CustomPageMaxLabelRunes) + "  "
		})},
		{"V6_url_2048_bytes", customPageWith(func(p *CustomPage) { p.URL = maxURL })},
		{"V6_scheme_http_uppercase", customPageWith(func(p *CustomPage) { p.URL = "HTTP://EXAMPLE.COM/Path" })},
		{"V6_scheme_https_uppercase", customPageWith(func(p *CustomPage) { p.URL = "HTTPS://Example.COM/path" })},
		{"V6_query_and_fragment", customPageWith(func(p *CustomPage) { p.URL = "https://example.com/p?a=1&b=2#frag" })},
		{"V6_port_and_localhost", customPageWith(func(p *CustomPage) { p.URL = "http://localhost:8080/x" })},
		{"V7_V8_all_enum_values", func() []CustomPage {
			var out []CustomPage
			for vi, visibility := range []CustomPageVisibility{CustomPageVisibilityAdmin, CustomPageVisibilityUser} {
				for ii, icon := range CustomPageIcons {
					page := customPageFixture()
					page.ID = fmt.Sprintf("page-%d%d", vi, ii)
					page.Visibility = visibility
					page.Icon = CustomPageIcon(icon)
					out = append(out, page)
				}
			}
			return out
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, ValidateCustomPages(tt.pages))
		})
	}
}

// TestParseCustomPagesRejectsInvalidUTF8 §19-2：非法字节必须**从 Parse 入口**就拒。
//
// 为什么不能只测 ValidateCustomPages 的非法 UTF-8（本文件已有一条 V5_label_invalid_utf8）：
// encoding/json 在 Unmarshal 阶段就把 0xff 静默替换成 U+FFFD，"替换后"的 label 是合法
// UTF-8，Validate 再也看不到问题——非法字节会以「看起来正常的替换字符」落库。所以这条
// 用例的输入必须是**原始字节串**（string([]byte{...,0xff,...})），断言点是 Parse 的返回
// error，而不是 Validate。
func TestParseCustomPagesRejectsInvalidUTF8(t *testing.T) {
	// 一条形状完全合法、只在 label 里含 0xff 的 JSON：去掉预检时它会解析成功。
	raw := string([]byte(`[{"id":"docs","label":"a` + "\xff" + `b","url":"https://example.com/"}]`))
	pages, err := ParseCustomPages(raw)
	require.Error(t, err, "非法 UTF-8 必须在 Unmarshal 之前被拒（M9 变异点）")
	require.Contains(t, err.Error(), "UTF-8", "错误必须指明原因，而不是含混的 invalid character")
	require.Nil(t, pages)

	// 同样的非法字节出现在 url 里也走同一条预检（预检是输入级，不是字段级）。
	rawURL := string([]byte(`[{"id":"docs","label":"Docs","url":"https://example.com/` + "\xff" + `"}]`))
	_, err = ParseCustomPages(rawURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "UTF-8")
}

// TestValidateCustomPagesRejects V2–V8 的每个拒绝类各一条子用例。
// 断言带期望片段：既确认「拒了」，也确认「拒的原因对」——M1 变异（删掉
// scheme 判定）会让 javascript: 落到 host 判定上，只有片段断言才能抓到。
func TestValidateCustomPagesRejects(t *testing.T) {
	tooLongURL := "https://example.com/" + strings.Repeat("a", CustomPageMaxURLBytes)
	tests := []struct {
		name    string
		pages   []CustomPage
		wantErr string
	}{
		{"V2_21_items", customPageList(CustomPageMaxItems + 1), "exceeds max 20"},

		{"V3_id_empty", customPageWith(func(p *CustomPage) { p.ID = "" }), "id"},
		{"V3_id_uppercase", customPageWith(func(p *CustomPage) { p.ID = "Docs" }), "id"},
		{"V3_id_leading_dash", customPageWith(func(p *CustomPage) { p.ID = "-a" }), "id"},
		{"V3_id_space", customPageWith(func(p *CustomPage) { p.ID = "a b" }), "id"},
		{"V3_id_too_long", customPageWith(func(p *CustomPage) { p.ID = strings.Repeat("a", CustomPageMaxIDBytes+1) }), "32 bytes"},

		{"V4_duplicate_id", []CustomPage{customPageFixture(), customPageFixture()}, "duplicate id"},

		{"V5_label_blank", customPageWith(func(p *CustomPage) { p.Label = "   " }), "label is required"},
		{"V5_label_too_long", customPageWith(func(p *CustomPage) { p.Label = strings.Repeat("汉", CustomPageMaxLabelRunes+1) }), "max 40"},
		{"V5_label_invalid_utf8", customPageWith(func(p *CustomPage) { p.Label = "a\xffb" }), "UTF-8"},

		{"V6_url_empty", customPageWith(func(p *CustomPage) { p.URL = "" }), "url is required"},
		{"V6_url_javascript", customPageWith(func(p *CustomPage) { p.URL = "javascript:alert(1)" }), "http or https"},
		{"V6_url_data", customPageWith(func(p *CustomPage) { p.URL = "data:text/html,<script>alert(1)</script>" }), "http or https"},
		{"V6_url_file", customPageWith(func(p *CustomPage) { p.URL = "file:///etc/passwd" }), "http or https"},
		{"V6_url_vbscript", customPageWith(func(p *CustomPage) { p.URL = "vbscript:msgbox(1)" }), "http or https"},
		{"V6_url_relative", customPageWith(func(p *CustomPage) { p.URL = "/relative" }), "http or https"},
		{"V6_url_protocol_relative", customPageWith(func(p *CustomPage) { p.URL = "//example.com/x" }), "http or https"},
		{"V6_url_userinfo", customPageWith(func(p *CustomPage) { p.URL = "https://u:p@example.com/" }), "userinfo"},
		// 空 userinfo：url.User 非 nil（Username/Password 都是空串）→ 同样拒。前端按
		// authority 段是否含 `@` 判（§19-4-1），这条锁住"Go 侧确实拒"这个前提。
		{"V6_url_empty_userinfo", customPageWith(func(p *CustomPage) { p.URL = "https://@example.com/" }), "userinfo"},
		// 非法百分号转义：url.Parse 报 invalid URL escape；前端补了同一条判定（§19-4-2）。
		{"V6_url_invalid_percent_escape", customPageWith(func(p *CustomPage) { p.URL = "https://example.com/%zz" }), "url is not a valid URL"},
		{"V6_url_newline", customPageWith(func(p *CustomPage) { p.URL = "https://example.com/a\nb" }), "control"},
		{"V6_url_trailing_newline", customPageWith(func(p *CustomPage) { p.URL = "https://example.com/a\n" }), "control"},
		{"V6_url_too_long", customPageWith(func(p *CustomPage) { p.URL = tooLongURL }), "2048"},
		{"V6_url_missing_host", customPageWith(func(p *CustomPage) { p.URL = "https://" }), "host"},
		{"V6_url_space_in_host", customPageWith(func(p *CustomPage) { p.URL = "https://exa mple.com/" }), "url is not a valid URL"},

		{"V7_visibility_invalid", customPageWith(func(p *CustomPage) { p.Visibility = "all" }), "visibility"},
		{"V7_visibility_empty", customPageWith(func(p *CustomPage) { p.Visibility = "" }), "visibility"},

		{"V8_icon_invalid", customPageWith(func(p *CustomPage) { p.Icon = "rocket" }), "icon"},
		{"V8_icon_empty", customPageWith(func(p *CustomPage) { p.Icon = "" }), "icon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCustomPages(tt.pages)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Contains(t, err.Error(), "custom pages", "错误必须能定位到配置项")
		})
	}
}

// TestValidateCustomPagesRejectsWithIndex 条目级错误必须带下标（§5：管理面要
// 指出第几条），重复 id 报的是第二次出现的下标。
func TestValidateCustomPagesRejectsWithIndex(t *testing.T) {
	pages := customPageList(3)
	pages[1].URL = "javascript:alert(1)"
	err := ValidateCustomPages(pages)
	require.Error(t, err)
	require.Contains(t, err.Error(), "custom pages[1]:")

	pages = []CustomPage{customPageFixture(), customPageFixture()}
	err = ValidateCustomPages(pages)
	require.Error(t, err)
	require.Contains(t, err.Error(), "custom pages[1]:")
	require.Contains(t, err.Error(), "first at index 0")
}

// TestValidateCustomPagesV9EncodedSize V9 是序列化后的总字节上界。字段级上限
// 允许 20×2048 字节 URL，但 JSON 转义会放大（`\` 翻倍）：20 条反斜杠路径的
// URL 编码后 ~80 KiB，必须在落库前拒掉。
func TestValidateCustomPagesV9EncodedSize(t *testing.T) {
	prefix := "https://example.com/"
	pages := make([]CustomPage, 0, CustomPageMaxItems)
	for i := 0; i < CustomPageMaxItems; i++ {
		page := customPageFixture()
		page.ID = fmt.Sprintf("p%02d", i)
		page.URL = prefix + strings.Repeat(`\`, CustomPageMaxURLBytes-len(prefix))
		pages = append(pages, page)
	}
	// 先确认这份构造没有踩到字段级规则——否则 V9 的断言可能被 V6 抢先满足。
	for i, page := range pages {
		require.NoError(t, validateCustomPage(page), "page %d", i)
	}
	encoded, err := MarshalCustomPages(pages)
	require.NoError(t, err)
	require.Greater(t, len(encoded), CustomPageMaxTotalBytes, "构造必须真的超过 64 KiB 上界")

	err = ValidateCustomPages(pages)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds max 65536")
}

// TestNormalizeCustomPages 归一化：补缺省、trim label、幂等、不改入参。
func TestNormalizeCustomPages(t *testing.T) {
	raw := `[{"id":"docs","label":"  Docs  ","url":"https://example.com/docs"},` +
		`{"id":"blog","label":"Blog","url":"https://blog.example.com","visibility":"admin","icon":"book","enabled":false}]`
	pages, err := ParseCustomPages(raw)
	require.NoError(t, err)

	normalized := NormalizeCustomPages(pages)
	require.Equal(t, "Docs", normalized[0].Label)
	require.Equal(t, CustomPageVisibilityUser, normalized[0].Visibility, "缺省 visibility=user")
	require.Equal(t, CustomPageIconLink, normalized[0].Icon, "缺省 icon=link")
	require.True(t, normalized[0].Enabled)
	require.Equal(t, CustomPageVisibilityAdmin, normalized[1].Visibility, "显式值必须保留")
	require.Equal(t, CustomPageIconBook, normalized[1].Icon)
	require.False(t, normalized[1].Enabled)

	// 幂等：写入路径会对已归一的值再跑一次，结果必须逐字段相同。
	require.Equal(t, normalized, NormalizeCustomPages(normalized))

	// 不改入参：读取路径传的是设置快照里的值，就地改会污染快照。
	require.Equal(t, "  Docs  ", pages[0].Label)
	require.Empty(t, pages[0].Visibility)
	require.Empty(t, pages[0].Icon)

	require.Nil(t, NormalizeCustomPages(nil))

	// 归一后的结果必须直接通过校验（service 的顺序契约：Parse→Normalize→Validate）。
	require.NoError(t, ValidateCustomPages(normalized))
}

// TestMarshalCustomPages 序列化：键序固定、无行尾换行、HTML 不转义、空值统一
// "[]"、往返幂等。字节相等是 §11-T5「库内值/返回值/快照值三者一致」的基础。
func TestMarshalCustomPages(t *testing.T) {
	pages := []CustomPage{{
		ID:         "docs",
		Label:      "文档",
		URL:        "https://example.com/docs?a=1&b=2#frag",
		Visibility: CustomPageVisibilityAdmin,
		Icon:       CustomPageIconChart,
		Enabled:    true,
	}}
	got, err := MarshalCustomPages(pages)
	require.NoError(t, err)
	require.Equal(t,
		`[{"id":"docs","label":"文档","url":"https://example.com/docs?a=1&b=2#frag","visibility":"admin","icon":"chart","enabled":true}]`,
		got)
	require.NotContains(t, got, "\n", "设置值不带行尾换行")
	require.NotContains(t, got, `\u0026`, "& 不能被转义（SetEscapeHTML(false)）")

	for _, empty := range [][]CustomPage{nil, {}} {
		got, err := MarshalCustomPages(empty)
		require.NoError(t, err)
		require.Equal(t, "[]", got)
	}

	// 往返：序列化 → 解析 → 再序列化，字节不变。
	roundTrip, err := ParseCustomPages(got)
	require.NoError(t, err)
	again, err := MarshalCustomPages(roundTrip)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

// TestCustomPagesParseNormalizeValidateMarshalChain 把四个入口按 service 的
// 真实顺序串一遍（非规范输入 → 规范字节串），确保单点入口之间没有缝。
func TestCustomPagesParseNormalizeValidateMarshalChain(t *testing.T) {
	raw := `[{"url":"https://example.com/docs","label":"  Docs  ","id":"docs"},` +
		`{"id":"blog","label":"Blog","url":"https://blog.example.com","enabled":true,"icon":"globe","visibility":"user"}]`
	pages, err := ParseCustomPages(raw)
	require.NoError(t, err)
	pages = NormalizeCustomPages(pages)
	require.NoError(t, ValidateCustomPages(pages))
	encoded, err := MarshalCustomPages(pages)
	require.NoError(t, err)
	require.Equal(t,
		`[{"id":"docs","label":"Docs","url":"https://example.com/docs","visibility":"user","icon":"link","enabled":true},`+
			`{"id":"blog","label":"Blog","url":"https://blog.example.com","visibility":"user","icon":"globe","enabled":true}]`,
		encoded)
}
