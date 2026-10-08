// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build unix

package usage

import "syscall"

// probeDiskFree 返回 path 所在卷的可用字节数（best-effort）：statfs 失败 ⇒ ok=false。
// 供 retention §3.11 容量 ETA/处置采样。
func probeDiskFree(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
