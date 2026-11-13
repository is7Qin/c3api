// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package logx

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

// failingSink 的 Write 恒失败，用来触发 zap 的内部写错误路径。
type failingSink struct{}

func (failingSink) Write([]byte) (int, error) { return 0, errors.New("sink boom") }
func (failingSink) Sync() error               { return nil }

// TestSinkWriteErrorReachesErrorOutput 守住「sink 写失败不再静默」：zap 内部
// 错误必须送到注入的 errOut（New 里即 stderr）。b8c57be4 的旧实现未设
// ErrorOutputPaths，这类错误被空 MultiWriteSyncer 吞掉。
//
// 断言子串取自实测输出 "<时间> write error: sink boom\n"：去掉易变的时间前缀，
// 保留 zap 的固定格式 "write error: " 与注入的错误文本，证明落到 errOut 的正是
// 这次 sink 写失败。
func TestSinkWriteErrorReachesErrorOutput(t *testing.T) {
	var errOut bytes.Buffer
	l := build(zapcore.InfoLevel, failingSink{}, func() {}, zapcore.AddSync(&errOut))

	l.Info("hello")

	require.Contains(t, errOut.String(), "write error: sink boom")
}
