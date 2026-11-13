// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package logx_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/logx"
)

// newFileLogger creates a logger that writes JSON lines to a fresh temp
// file and returns the logger plus the file path. The sink is closed via
// t.Cleanup before the test's TempDir is removed: on Windows an open sink
// makes the file undeletable.
func newFileLogger(t *testing.T, level string) (*logx.Logger, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.json")
	logger, err := logx.New(level, out)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, logger.Close()) })
	return logger, out
}

// readJSONLines 读取并按行解析日志文件，断言每行都是完整 JSON。
func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var recs []map[string]any
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(ln), &rec), "每行必须是完整 JSON：%s", ln)
		recs = append(recs, rec)
	}
	return recs
}

func keysOf(rec map[string]any) []string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(raw), "visible")
	require.NotContains(t, string(raw), "hidden")
}

func TestWithFields(t *testing.T) {
	logger, out := newFileLogger(t, "error")
	child := logger.With(logx.String("trace", "abc"))
	child.Error("boom", logx.Error(nil))
	require.NoError(t, logger.Sync())

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"trace":"abc"`)
	require.Contains(t, string(raw), "boom")
}

// TestCloseReleasesFile 是「Windows 上无法删除临时日志文件」的回归守卫：
// Close 之后文件必须能被 os.Remove，且内容完整可读。
func TestCloseReleasesFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "probe.log")
	logger, err := logx.New("info", out)
	require.NoError(t, err)

	logger.Info("first", logx.String("k", "v"))
	logger.Warn("second")

	require.NoError(t, logger.Close())
	// 可重复关闭：第二次不得报错也不得 panic。
	require.NoError(t, logger.Close())

	recs := readJSONLines(t, out)
	require.Len(t, recs, 2, "Close 后两行日志必须都已落盘")
	require.Equal(t, "first", recs[0]["msg"])
	require.Equal(t, "warn", recs[1]["level"])

	require.NoError(t, os.Remove(out), "Close 之后 Windows 必须能删除该文件")
	_, err = os.Stat(out)
	require.True(t, os.IsNotExist(err), "文件必须已被删除")
}

// TestCloseWithoutFileSink：stdout 输出与 nil logger 的 Close 都必须安全。
func TestCloseWithoutFileSink(t *testing.T) {
	logger, err := logx.New("info", "")
	require.NoError(t, err)
	logger.Info("to stdout")
	require.NoError(t, logger.Close())

	var nilLogger *logx.Logger
	require.NoError(t, nilLogger.Close())
}

// TestJSONShapeAndCallerSkip 锁定编码契约与 caller skip：键名
// msg/level/ts/caller 不变（Error 级也不带 stacktrace），caller 指向业务
// 调用点而不是本包的包装层。
func TestJSONShapeAndCallerSkip(t *testing.T) {
	logger, out := newFileLogger(t, "info")

	var infoLine int
	_, _, infoLine, _ = runtime.Caller(0)
	logger.Info("hello", logx.String("k", "v"))
	logger.Error("failure", logx.Error(errors.New("boom")))
	require.NoError(t, logger.Sync())

	recs := readJSONLines(t, out)
	require.Len(t, recs, 2)

	info := recs[0]
	require.Equal(t, []string{"caller", "k", "level", "msg", "ts"}, keysOf(info))
	require.Equal(t, "hello", info["msg"])
	require.Equal(t, "info", info["level"])
	require.Equal(t, "v", info["k"])
	// ISO8601：本地时区偏移或 Z。
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}(Z|[+-]\d{4})$`, info["ts"])

	caller, _ := info["caller"].(string)
	require.True(t,
		strings.HasSuffix(caller, "logx_test.go:"+strconv.Itoa(infoLine+1)),
		"caller 必须是业务调用点 %d，实际 %q", infoLine+1, caller)

	// Error 级：与改前等价——StacktraceKey 已对齐 zap 默认值，但本包不启用
	// AddStacktrace，所以记录里没有 stacktrace 字段（键集是精确断言 ⇒ 一旦有人
	// 打开 AddStacktrace 而没有同步本契约，这里立刻红）。error 字段仍在。
	require.Equal(t, []string{"caller", "error", "level", "msg", "ts"}, keysOf(recs[1]))
	require.Equal(t, "error", recs[1]["level"])
	require.Equal(t, "boom", recs[1]["error"])
}

// TestInvalidLevelRejected：level 解析语义不变。
func TestInvalidLevelRejected(t *testing.T) {
	_, err := logx.New("nope", "")
	require.Error(t, err)
}
