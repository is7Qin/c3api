// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package logx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/logx"
)

// newFileLogger creates a logger that writes JSON lines to a fresh temp
// file and returns the logger plus the file path. On Windows, zap keeps
// the sink file open for the process lifetime, so the dir cleanup is
// best-effort there (Linux/macOS remove it right away).
func newFileLogger(t *testing.T, level string) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "logx-test-")
	require.NoError(t, err)
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New(level, out)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logger, out
}

func TestDebugEnabled(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  bool
	}{
		{"debug", true},
		{"info", false},
		{"warn", false},
	} {
		t.Run(tc.level, func(t *testing.T) {
			logger, _ := newFileLogger(t, tc.level)
			require.Equal(t, tc.want, logger.DebugEnabled())
		})
	}
}

func TestLevelFiltering(t *testing.T) {
	// warn 级别下 Debug 不输出、Warn 输出
	logger, out := newFileLogger(t, "warn")
	logger.Debug("hidden", logx.String("k", "v"))
	logger.Warn("visible", logx.Int("n", 1))
	require.NoError(t, logger.Sync())

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "visible")
	require.NotContains(t, string(b), "hidden")
}

func TestWithFields(t *testing.T) {
	logger, out := newFileLogger(t, "error")
	child := logger.With(logx.String("trace", "abc"))
	child.Error("boom", logx.Error(nil))
	require.NoError(t, logger.Sync())

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), `"trace":"abc"`)
	require.Contains(t, string(b), "boom")
}
