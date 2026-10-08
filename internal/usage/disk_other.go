// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build !unix

package usage

// probeDiskFree 非 unix 平台无 statfs（占位）：ok=false ⇒ 容量 ETA unknown。
func probeDiskFree(string) (int64, bool) { return 0, false }
