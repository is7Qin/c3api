// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAccountFieldSpecsMatchGeneratedPatch 是**契约与声明表的漂移门**：写面字段
// 的唯一事实来源是 domain 的声明表，而对外契约由 openapi 生成到
// AccountConfigPatch。二者一旦漂移（契约加了字段而表没加，或反之），字段的
// 类别（身份/配置）就无人声明 ⇒ 该字段既不推进 K 也不参与失效，静默变成
// 陈旧读。本用例按符号解析生成文件，机械断言两侧字段集相等。
func TestAccountFieldSpecsMatchGeneratedPatch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "api.gen.go", nil, 0)
	require.NoError(t, err, "parse generated contract")

	var patch *ast.StructType
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "AccountConfigPatch" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if ok {
			patch = st
		}
		return false
	})
	require.NotNil(t, patch, "AccountConfigPatch must exist in the generated contract")

	contractFields := make([]string, 0, len(patch.Fields.List))
	for _, f := range patch.Fields.List {
		require.NotNil(t, f.Tag, "contract field %v has no tag", f.Names)
		tag, err := strconv.Unquote(f.Tag.Value)
		require.NoError(t, err)
		name := reflect.StructTag(tag).Get("json")
		require.NotEmpty(t, name, "contract field %v has no json tag", f.Names)
		contractFields = append(contractFields, name[:len(name)-len(",omitempty")])
	}

	declared := make([]string, 0, len(domain.AccountFieldSpecs()))
	for _, spec := range domain.AccountFieldSpecs() {
		declared = append(declared, spec.Name)
	}
	sort.Strings(contractFields)
	sort.Strings(declared)
	require.Equal(t, declared, contractFields,
		"declared account fields must match the generated write contract exactly")
}
