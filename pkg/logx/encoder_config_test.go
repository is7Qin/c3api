// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package logx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEncoderConfigKeys 钉住日志字段契约的键名（含 StacktraceKey 对齐）：键名
// 一改，/logs 消费方与既有 grep/告警会静默失效，所以必须是显式门禁。
//
// 编码器是函数值（不可比较），只断言装配点非 nil；level/ts/caller 的具体形态由
// 渲染用例（logx_test.TestJSONShapeAndCallerSkip）证明——小写 level、ISO8601、
// 短 caller 与业务调用点行号。
func TestEncoderConfigKeys(t *testing.T) {
	cfg := encoderConfig()
	require.Equal(t, "msg", cfg.MessageKey)
	require.Equal(t, "level", cfg.LevelKey)
	require.Equal(t, "ts", cfg.TimeKey)
	require.Equal(t, "caller", cfg.CallerKey)
	require.Equal(t, "stacktrace", cfg.StacktraceKey,
		"与 zap 默认生产配置对齐；是否输出由 AddStacktrace 决定（当前未启用）")
	require.NotNil(t, cfg.EncodeLevel)
	require.NotNil(t, cfg.EncodeTime)
	require.NotNil(t, cfg.EncodeCaller)
}
