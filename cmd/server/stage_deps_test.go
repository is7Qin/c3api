// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stageDepsBaseline 冻结各阶段窄结构的字段数（spec 2026-11-18 §7.3）。
//
// 为什么冻结数字而不是断言「比原 Face 小」：`newCodexFace` 横跨 infra/health/
// billing 三个来源，「比哪个 Face 小」都恒真，那种断言不可证伪。冻结字段数至少
// 让「悄悄把整个 Face 塞回来」或「顺手多接一个依赖」必须显式改这张表。
var stageDepsBaseline = map[string]int{
	"healthDeps":  11,
	"notifyDeps":  3,
	"billingDeps": 17,
	"codexDeps":   22,
}

// TestStageDepsAreNarrow 钉两件事：
//
//  1. 字段数与冻结表一致（新增依赖必须改表，diff 里看得见）；
//  2. 窄结构里**不允许出现任何 Face 指针**——否则窄化只是换了变量名，
//     后阶段照样能顺着 `*infraFace` 摸到全部产物。
func TestStageDepsAreNarrow(t *testing.T) {
	for name, want := range stageDepsBaseline {
		typ := stageDepsType(t, name)
		require.Equal(t, want, typ.NumField(),
			"%s 的字段数与冻结表不符：新增/删除依赖请同时更新 stageDepsBaseline 并在提交正文说明", name)
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if strings.HasSuffix(f.Type.String(), "Face") {
				t.Errorf("%s.%s 是 Face 指针（%s）：窄结构不得持有整个阶段产物，那会把宽面重新暴露出来",
					name, f.Name, f.Type)
			}
		}
	}
}

// stageDepsType 按名字取窄结构类型（未导出类型只能用字符串查，保持表驱动）。
func stageDepsType(t *testing.T, name string) reflect.Type {
	t.Helper()
	switch name {
	case "healthDeps":
		return reflect.TypeOf(healthDeps{})
	case "notifyDeps":
		return reflect.TypeOf(notifyDeps{})
	case "billingDeps":
		return reflect.TypeOf(billingDeps{})
	case "codexDeps":
		return reflect.TypeOf(codexDeps{})
	default:
		t.Fatalf("未知阶段窄结构 %q", name)
		return nil
	}
}
