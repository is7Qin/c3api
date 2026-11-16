// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件钉住 spec 2026-11-16 §11-T4（跨语言常量等号）。
//
// 为什么要有这道门禁：前端 `web/src/lib/custom-pages.ts` 有**第一份**图标清单与上限
// 常量（校验是 UX，服务端校验才是安全边界，§9-U7），Go 侧 `custompage.go` 有第二份。
// 两份靠人眼同步迟早漂移，而漂移的后果不对称：
//   - 图标清单少一个 → 编辑器下拉里选不到，后端却接受（或反之，提交即 400）；
//   - 上限常量不一致 → 前端放行、后端 400（管理员拿到一条看不懂的失败），或者前端
//     先拒了后端本可接受的配置（功能凭空变窄）。
//
// 断言口径：
//   - 图标清单**含顺序**逐位相等（顺序即编辑器下拉顺序，与字母序无关）；
//   - 4 个**扁平标量**逐一相等（TS 侧刻意不做对象字面量，就是为这里的正则匹配）；
//   - visibility 集合相等（顺序无意义，用集合比较）。
//
// 文件缺失 / 常量缺失 / 数组为空一律 **fail**：门禁"找不到比较对象就跳过"等于门禁
// 不存在，而这条路径恰恰是"前端文件被改名/搬走"时最先发生的事。
// 断言只用 require（失败即停）；不用 t.Parallel（纯读文件，没有并行收益）。

// customPagesTSRelPath 相对本包目录（internal/domain）的前端规则层路径。
const customPagesTSRelPath = "../../web/src/lib/custom-pages.ts"

// tsStringArrayRe 匹配 `export const NAME = [ ... ]`（数组元素只有字符串字面量，
// 所以 `[^\]]*` 不会提前截断）。
func tsStringArrayRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)export const ` + regexp.QuoteMeta(name) + `\s*=\s*\[([^\]]*)\]`)
}

// tsScalarRe 匹配 `export const NAME = 20`。
func tsScalarRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`export const ` + regexp.QuoteMeta(name) + `\s*=\s*(-?\d+)`)
}

// tsRegexLiteralRe 匹配 `export const NAME = /pattern/`（单行、无 flag）。
// 捕获组里就是正则**字面量本身**（不含两侧斜杠），用于与 Go 的 CustomPageIDPattern
// 做等号比对——比"两边都跑同一批用例"更直接：字面量一分叉立刻红。
func tsRegexLiteralRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`export const ` + regexp.QuoteMeta(name) + `\s*=\s*/([^/\n]+)/`)
}

// tsStringLiteralRe 从数组体里取每个单引号字面量。
var tsStringLiteralRe = regexp.MustCompile(`'([^']*)'`)

// readCustomPagesTS 读取前端规则层源码；读不到直接 fail，不 skip。
func readCustomPagesTS(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.FromSlash(customPagesTSRelPath))
	require.NoError(t, err)
	src, err := os.ReadFile(abs)
	require.NoError(t, err, "跨语言门禁读不到前端规则层：%s（文件被移动/改名即视为门禁失效）", abs)
	require.NotEmpty(t, src, "前端规则层是空文件：%s", abs)
	return string(src)
}

// tsStringArray 取 TS 侧字符串数组；常量缺失或数组为空都 fail。
func tsStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	match := tsStringArrayRe(name).FindStringSubmatch(src)
	require.NotNil(t, match, "web/src/lib/custom-pages.ts 里找不到 `export const %s = [...]`", name)
	literals := tsStringLiteralRe.FindAllStringSubmatch(match[1], -1)
	require.NotEmpty(t, literals, "`%s` 是空数组：门禁没有可比较的内容", name)
	out := make([]string, 0, len(literals))
	for _, literal := range literals {
		out = append(out, literal[1])
	}
	return out
}

// tsScalar 取 TS 侧整数标量；常量缺失或不是整数都 fail。
func tsScalar(t *testing.T, src, name string) int {
	t.Helper()
	match := tsScalarRe(name).FindStringSubmatch(src)
	require.NotNil(t, match, "web/src/lib/custom-pages.ts 里找不到 `export const %s = <数字>`", name)
	value, err := strconv.Atoi(match[1])
	require.NoError(t, err, "`%s` 不是可解析的整数：%q", name, match[1])
	return value
}

func TestCustomPageFrontendGateMatchesGoConstants(t *testing.T) {
	src := readCustomPagesTS(t)

	t.Run("icons 含顺序逐位相等", func(t *testing.T) {
		require.Equal(t, CustomPageIcons, tsStringArray(t, src, "CUSTOM_PAGE_ICONS"),
			"前端图标清单必须与 domain.CustomPageIcons 逐位相等（顺序即下拉顺序，别按字母序重排）")
	})

	t.Run("四个扁平上限标量逐一相等", func(t *testing.T) {
		require.Equal(t, CustomPageMaxItems, tsScalar(t, src, "CUSTOM_PAGE_MAX_ITEMS"), "CUSTOM_PAGE_MAX_ITEMS")
		require.Equal(t, CustomPageMaxLabelRunes, tsScalar(t, src, "CUSTOM_PAGE_MAX_LABEL"), "CUSTOM_PAGE_MAX_LABEL")
		require.Equal(t, CustomPageMaxURLBytes, tsScalar(t, src, "CUSTOM_PAGE_MAX_URL"), "CUSTOM_PAGE_MAX_URL")
		require.Equal(t, CustomPageMaxIDBytes, tsScalar(t, src, "CUSTOM_PAGE_MAX_ID"), "CUSTOM_PAGE_MAX_ID")
	})

	t.Run("visibilities 集合相等", func(t *testing.T) {
		require.ElementsMatch(t,
			[]string{string(CustomPageVisibilityAdmin), string(CustomPageVisibilityUser)},
			tsStringArray(t, src, "CUSTOM_PAGE_VISIBILITIES"),
			"前端 visibility 清单必须与 Go 的 admin/user 常量集合相等")
	})

	// §19-5：id 正则与 scheme 白名单此前只活在两端各自的校验函数里，是无门禁的
	// 第二份真源（改一端另一端静默漂移：前端放行、后端 400，或反之功能凭空变窄）。
	// 这里直接比对**字面量**：Go 的 CustomPageIDPattern / CustomPageURLSchemes 与
	// TS 的 CUSTOM_PAGE_ID_PATTERN / CUSTOM_PAGE_URL_SCHEMES。
	t.Run("id 正则字面量两端等号", func(t *testing.T) {
		match := tsRegexLiteralRe("CUSTOM_PAGE_ID_PATTERN").FindStringSubmatch(src)
		require.NotNil(t, match,
			"web/src/lib/custom-pages.ts 里找不到 `export const CUSTOM_PAGE_ID_PATTERN = /.../`：正则必须是可比较的字面量常量")
		require.Equal(t, CustomPageIDPattern, match[1],
			"前端 id 正则必须与 domain.CustomPageIDPattern 逐字符相等")
	})

	t.Run("scheme 白名单两端等号", func(t *testing.T) {
		require.Equal(t, CustomPageURLSchemes, tsStringArray(t, src, "CUSTOM_PAGE_URL_SCHEMES"),
			"前端 scheme 白名单必须与 domain.CustomPageURLSchemes 逐位相等（含顺序）")
	})
}
