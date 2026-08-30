// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/pkg/logx"
)

// logSnapshotReloadErr 逐快照 reload 失败的 operator 结构化日志（startup/
// scope/FullRefresh 三处共用，spec §5.4）：恒带 snapshot 与 error 字段；panic
// 错误经 snapshot.PanicStackForLog 追加一次性 stack 字段（operator-only——
// 不进 message、Status、HTTP projection，raw panic value 永不出注册表）。
// log nil = 静默（测试装配）。
func logSnapshotReloadErr(log *logx.Logger, msg, name string, err error) {
	if log == nil {
		return
	}
	fields := []logx.Field{logx.String("snapshot", name), logx.Error(err)}
	if stack, ok := snapshot.PanicStackForLog(err); ok {
		fields = append(fields, logx.String("stack", stack))
	}
	log.Warn(msg, fields...)
}
