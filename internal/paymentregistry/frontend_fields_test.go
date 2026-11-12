// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package paymentregistry

// frontend_fields_test.go P50②（spec v2 §13 / §19-4a）：**前端字段清单的 Go 侧等号门禁**。
//
// 为什么方向是"前端留清单、Go 来核对"（N62）：Docker 的 web 阶段只 `COPY web/`，
// Go 常量在容器里不存在，所以前端不可能在构建期读到 `epay.ConfigField*`；反过来
// Go 测试能读到仓库里的 `web/src/lib/payment-fields.ts`（gobuild 阶段 `COPY . .`
// 整个仓库都在）。
//
// 三方等号（缺一条就会留一条漂移路径）：
//
//	① TS 清单  ==  `epay.ConfigField*` 常量     —— 前端少/多一个字段即红（M43）
//	② 常量      ==  `epay.Provider.ConfigSchema()` 的字段名 —— 常量自己不许漂
//	③ TS 协议名 ==  `Registry(nil).All()` 的名字 —— 协议改名/新增未同步即红
//
// 为什么解析失败要直接 fail（不再 skip）：门禁读不到被核对对象时静默变绿，等于
// 没有门禁。列表为空同理（空清单能让 ① 恒真）。

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service/epay"
)

// frontendFieldsRel 前端字段清单（唯一被允许保留协议字段名的那份文件）。
// 路径相对本包（`go test` 的 cwd 永远是包目录）。
const frontendFieldsRel = "../../web/src/lib/payment-fields.ts"

// TestFrontendPaymentConfigFieldsMirror P50②：字段名三方等号。
func TestFrontendPaymentConfigFieldsMirror(t *testing.T) {
	ts := readFrontendFields(t)

	tsFields := tsStringArray(t, ts, "PAYMENT_CONFIG_FIELDS")
	// ① TS ↔ epay 常量（顺序也取等号：它是表单字段的声明顺序，前端不得重排）。
	wantFields := []string{
		epay.ConfigFieldBaseURL,
		epay.ConfigFieldPID,
		epay.ConfigFieldPrivateKey,
		epay.ConfigFieldPublicKey,
	}
	require.Equal(t, wantFields, tsFields,
		"%s 的 PAYMENT_CONFIG_FIELDS 必须与 epay.ConfigField* 常量逐项相等（顺序即表单顺序）",
		frontendFieldsRel)

	// ② 常量 ↔ 真实协议的 schema：常量不能自成一份真相。
	registry, err := Registry(nil)
	require.NoError(t, err)
	epayProvider, ok := registry.Get(domain.ProviderEpayV2)
	require.True(t, ok, "epay 必须在注册表里（本门禁的核对基准）")
	schema := epayProvider.ConfigSchema()
	schemaNames := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		schemaNames = append(schemaNames, f.Name)
	}
	require.Equal(t, wantFields, schemaNames,
		"epay.ConfigSchema() 的字段名必须与 ConfigField* 常量一致（两边都不许单独改）")

	// 全仓视角：所有注册协议的字段名并集必须被 TS 清单覆盖（新增协议时忘改清单 → 红）。
	all := make(map[string]struct{}, len(wantFields))
	for _, p := range registry.All() {
		for _, f := range p.ConfigSchema().Fields {
			all[f.Name] = struct{}{}
		}
	}
	for name := range all {
		require.Contains(t, tsFields, name,
			"注册协议声明了 config 字段 %q，但 %s 的清单里没有它（表单会少一个框）", name, frontendFieldsRel)
	}
	require.Len(t, tsFields, len(all),
		"TS 清单与所有注册协议的字段并集必须等势（多出来的项同样会漂移）")
}

// TestFrontendPaymentProviderNamesMirror P50②：协议名等号。
//
// 前端只需要知道"本二进制有哪些协议"这一件事来给下拉兜底/自检；真正的协议列表
// 来自元数据端点。这里钉的是"清单不许漏、不许编"。
func TestFrontendPaymentProviderNamesMirror(t *testing.T) {
	ts := readFrontendFields(t)
	tsNames := tsStringArray(t, ts, "PAYMENT_PROVIDER_NAMES")

	registry, err := Registry(nil)
	require.NoError(t, err)
	providers := registry.All()
	require.NotEmpty(t, providers, "0 个协议时本门禁会空转成恒绿")

	want := make([]string, 0, len(providers))
	for _, p := range providers {
		want = append(want, string(p.Name()))
	}
	// All() 已按 name 升序（构造期定死）；TS 清单按同一顺序书写，这里不做排序。
	require.Equal(t, want, tsNames,
		"%s 的 PAYMENT_PROVIDER_NAMES 必须等于 Registry(nil).All() 的协议名（升序）",
		frontendFieldsRel)
}

// readFrontendFields 读清单文件的原文；缺失即 fail（不许 skip）。
func readFrontendFields(t *testing.T) string {
	t.Helper()
	path := filepath.FromSlash(frontendFieldsRel)
	raw, err := os.ReadFile(path)
	require.NoError(t, err,
		"读不到 %s：P50② 的核对对象不存在时不得静默变绿（spec §19-4a）", frontendFieldsRel)
	require.NotEmpty(t, raw, "%s 是空文件（清单被清空同样等于没有门禁）", frontendFieldsRel)
	return string(raw)
}

// tsStringArray 解析 `export const NAME = ['a', 'b'] as const` 里的字符串数组。
//
// 刻意用正则而不是引入 TS 解析器：被核对对象是一行字面量常量，正则足够；解析不到
// 就是红（不是"跳过这一项"）。
func tsStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	decl := regexp.MustCompile(`(?s)export\s+const\s+` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]`)
	m := decl.FindStringSubmatch(src)
	require.Len(t, m, 2,
		"在 %s 里找不到 `export const %s = [...]`：门禁的核对对象必须可解析（§19-4a）",
		frontendFieldsRel, name)

	item := regexp.MustCompile(`'([^']*)'|"([^"]*)"`)
	matches := item.FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(matches))
	for _, mm := range matches {
		out = append(out, mm[1]+mm[2])
	}
	require.NotEmpty(t, out, "%s 的 %s 不得为空数组（空清单会让等号门禁恒真）", frontendFieldsRel, name)
	return out
}
