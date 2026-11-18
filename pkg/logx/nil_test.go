// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package logx_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/logx"
)

// TestNilLoggerIsSilent 钉住 P0 的契约：nil 接收者的四个级别、DebugEnabled、
// With、Sync 一律不 panic。这是全仓删除 `if x.log != nil` 守卫的前提——守卫
// 一删，任何一处 nil logger 打日志都会变成 panic，所以这条测试是那批删除的
// 唯一安全网。
func TestNilLoggerIsSilent(t *testing.T) {
	var l *logx.Logger

	require.NotPanics(t, func() {
		l.Debug("d", logx.String("k", "v"))
		l.Info("i", logx.String("k", "v"))
		l.Warn("w", logx.String("k", "v"))
		l.Error("e", logx.String("k", "v"))
	})

	require.False(t, l.DebugEnabled(), "空 logger 必须报 debug 不可用")
	require.Nil(t, l.With(logx.String("k", "v")), "空 logger 的 With 必须返回 nil 以保持链式静默")
	require.NoError(t, l.Sync())

	// 链式到派生 logger 仍静默。
	require.NotPanics(t, func() { l.With(logx.String("k", "v")).Info("i") })
}

// TestZeroLoggerIsSilent 覆盖「给了 logger 但没构造完」的零值形态。
//
// 与 nil 接收者是两件事：`&logx.Logger{}` 非 nil，`l == nil` 判不住它，必须
// 另外判内部 *zap.Logger 为 nil。logx 是导出包、Logger 是导出类型，零值必须
// 有定义，否则任何 `var lg logx.Logger` 或结构体零值都会在打日志时 panic。
func TestZeroLoggerIsSilent(t *testing.T) {
	l := &logx.Logger{}

	require.NotPanics(t, func() {
		l.Debug("d")
		l.Info("i")
		l.Warn("w")
		l.Error("e")
	})

	require.False(t, l.DebugEnabled())
	require.Nil(t, l.With(logx.String("k", "v")))
	require.NoError(t, l.Sync())
	require.NoError(t, l.Close())
}
