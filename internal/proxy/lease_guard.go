// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import "github.com/is7qin/c3api/internal/api"

// leaseGuard ensures exact lease release on panic after real Select.
// Shared production proxy lease owner uses defer/recover-safe guard so panic
// after real Select releases exact token once (leaseToken CAS idempotent).
//
// nil 语义与 P3b 前逐字一致：没有 selection（预留失败前就 defer 了本函数）⇒
// 两次调用都是 no-op。落点从「(*Selection).Release 的 nil receiver 早退」换成契约的
// HandleOrDetached（nil selection ⇒ 分离句柄）：调用形态与判空点都不变。
func leaseGuard(sel *api.Selection) {
	if r := recover(); r != nil {
		sel.HandleOrDetached().Release()
		panic(r)
	}
	sel.HandleOrDetached().Release()
}
