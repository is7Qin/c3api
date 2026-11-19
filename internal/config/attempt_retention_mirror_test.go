// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

// attempt_logs 保留期的「默认值 ↔ 文档镜像」锚（spec 2026-11-18 retry-observability
// §6 的 S2 验收 6）。
//
// 为什么需要它：`attempt_retention_days` 只在 `defaults()` 里有一个真值，而
// config.example.toml / deploy/config.toml 里是**注释行**（不配就走默认）。
// 两处注释写「默认 7 天」而代码改成 5，运维照着文档估算容量就会错——这种漂移
// 没有任何编译期/运行期信号。变异评审的正是 `defaults()` 的 7 → 0。

import (
	"os"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// attemptRetentionDefaultDocs 是声明 attempt 保留期默认值的配置镜像文件。
var attemptRetentionDefaultDocs = []string{
	"../../config.example.toml",
	"../../deploy/config.toml",
}

// attemptRetentionKeyRe 匹配「（注释或生效的）attempt_retention_days = N」。
var attemptRetentionKeyRe = regexp.MustCompile(`(?m)^\s*#?\s*attempt_retention_days\s*=\s*(\d+)`)

func TestAttemptRetentionDefaultMirrorsDocs(t *testing.T) {
	want := defaults().Usage.AttemptRetentionDays
	require.Equal(t, 7, want, "默认 7 天（与 err_logs 同档）")

	for _, path := range attemptRetentionDefaultDocs {
		body, err := os.ReadFile(path)
		require.NoError(t, err, "配置镜像必须存在：%s", path)
		m := attemptRetentionKeyRe.FindStringSubmatch(string(body))
		require.NotNil(t, m, "%s 必须声明 attempt_retention_days（注释行也算：它是默认值的文档面）", path)
		got, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		require.Equal(t, want, got,
			"%s 的 attempt_retention_days 与 defaults() 不一致：改默认值必须同时改两处镜像", path)
	}
}

// TestAttemptLogQueueKnobsStayOutOfConfig 落盘节奏不是配置项：attempt_logs 的
// 队列/批量/节奏由 worker 常量决定（写面不变量），只有保留期可配。这条门防的是
// 「顺手把 errlog_* 那三个键复制过来」——一旦进 config，就得为它们写校验、
// 文档与兼容性承诺，而它们的正确值只随表设计变。
func TestAttemptLogQueueKnobsStayOutOfConfig(t *testing.T) {
	keys := usageKoanfKeys(t)
	require.NotContains(t, keys, "attemptlog_queue_size")
	require.NotContains(t, keys, "attemptlog_batch_size")
	require.NotContains(t, keys, "attemptlog_flush_interval")
	require.Contains(t, keys, "attempt_retention_days", "保留期是唯一的可配项")
}

// usageKoanfKeys 反射取 UsageConfig 的 koanf 键集合（键是契约：改键等于改运维接口）。
func usageKoanfKeys(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf(UsageConfig{})
	keys := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("koanf")
		require.NotEmpty(t, tag, "UsageConfig.%s 缺 koanf tag", typ.Field(i).Name)
		keys = append(keys, tag)
	}
	return keys
}
