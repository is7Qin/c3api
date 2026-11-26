// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"github.com/is7qin/c3api/internal/identityenv"
	"github.com/is7qin/c3api/internal/modelmap"
)

// 编译期证明：本包这几个名字与中立叶子包里的类型**同一**（别名），不是新类型。
//
// 为什么必须钉住：别名少写一个 `=` 就变成 defined type。那之后编译器在多数
// 位置仍会工作（Go 允许 defined type 与其底层类型之间显式转换），只要有谁在
// 跨包边界补一次转换，整个包就自洽了——**named type 逃逸对编译器是隐形的**。
// 实测过这个逃逸场景：漏 `=` 且在边界加一处 ModelMapping(entM)，`go build` 全绿。
//
// 判据是**无转换的直接赋值**：显式转换对 defined type 恒成立，证明不了任何东西；
// 直接赋值要求两边是同一个类型，漏 `=` 时在此必然编译失败
// （`cannot use ModelMapping(nil) (value of map type …) as modelmap.ModelMapping value`）。
//
// 这里刻意是**赋值语句**（触发类型检查），不是 `var _ = f`（只取函数值、不调用、
// 是死代码）。也刻意不放 func init() + panic：那会让生产进程为开发者失误在启动期
// 崩溃，而这条断言拦的是编译期该拦的事。
var (
	_ modelmap.ModelMapping      = ModelMapping{}
	_ modelmap.ModelMappingEntry = ModelMappingEntry{}
	_ modelmap.ModelMappingMode  = ModelMappingMode(0)

	_ identityenv.IdentityEnvelope = IdentityEnvelope(nil)
	_ identityenv.IdentityPatch    = IdentityPatch{}
)
