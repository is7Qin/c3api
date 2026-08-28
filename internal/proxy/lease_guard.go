// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import "github.com/is7qin/c3api/internal/scheduler"

// leaseGuard ensures exact lease release on panic after real Select.
// Shared production proxy lease owner uses defer/recover-safe guard so panic
// after real Select releases exact token once (leaseToken CAS idempotent).
func leaseGuard(sel *scheduler.Selection) {
	if r := recover(); r != nil {
		sel.Release()
		panic(r)
	}
	sel.Release()
}
