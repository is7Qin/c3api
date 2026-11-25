// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"reflect"
	"strings"
	"unicode"

	"github.com/is7qin/c3api/internal/repository"
)

// splitAndTrim 把 SQL 的 `a, b, c` 拆成 [a b c]。
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// routingQualityRowFieldNames 用反射取 canonical 行类型的导出字段名。
// 用反射而不是手抄清单：加字段时断言自动要求 SQL 同步，而不是让人维护两份。
func routingQualityRowFieldNames(row repository.RoutingQualityRow) []string {
	rt := reflect.TypeOf(row)
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out = append(out, rt.Field(i).Name)
	}
	return out
}

// snakeCase 把 Go 的 CamelCase / 缩写（TTFTHist、Count429、Count5xx）转成
// routing_quality_fact 的列名风格。规则只覆盖本表实际用到的形状：
// 大写字母边界插 `_`、连续大写段的最后一个字母属于下一段、数字不另起段。
func snakeCase(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			prevIsLowerOrDigit := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if i > 0 && (prevIsLowerOrDigit || nextIsLower) && runes[i-1] != '_' {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
