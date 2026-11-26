// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import "github.com/is7qin/c3api/internal/domain"

// 编译期证明：本包的 AssignmentKey 与 domain.AssignmentKey 是**同一个类型**
// （别名），不是新类型。
//
// 判据是**无转换的直接赋值**：Go 允许 defined type 与其底层类型之间显式转换，
// 所以写成 domain.AssignmentKey(AssignmentKey{}) 永远为真、证明不了任何东西；
// 而直接赋值要求两边是同一个类型，漏写 `=` 变成 defined type 时在这里必然
// 编译失败（`cannot use AssignmentKey{} (value of struct type ...) as
// domain.AssignmentKey value`）。
//
// 这里刻意是**赋值语句**（触发类型检查），不是 `var _ = AssignmentKey{}`
// （只求值、不比较，是死代码）。
var _ domain.AssignmentKey = AssignmentKey{}
