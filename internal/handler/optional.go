// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// optionalStr 读侧「非空即指针」：空串 = 字段缺席 ⇒ nil；它与输入侧 nullableScalar
// 的三态**对称**（缺席 ⇔ nil、null ⇔ &""、有值 ⇔ &v），线格式因此只需 present/absent
// 一处判据。
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
