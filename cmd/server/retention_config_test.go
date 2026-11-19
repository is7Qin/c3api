// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

// 保留期配置投影锚（spec 2026-11-18 retry-observability §6 的 S2 验收 6 的装配面）。
//
// 为什么需要：retention worker 的五个天数来自五处 config 字段，装配时漏一个的
// 后果是「那张表永不清理」——没有崩溃、没有告警，只是分区无限增长。变异评审的
// 正是 `AttemptRetentionDays: cfg.Usage.AttemptRetentionDays` → `0`：**全绿**。
// 所以这里用五个互不相同的哨兵值，逐字段钉住投影。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
)

func TestRetentionConfigFromProjectsEveryField(t *testing.T) {
	cfg := &config.Config{}
	cfg.Usage.LogRetentionDays = 11
	cfg.Usage.ErrLogRetentionDays = 12
	cfg.Usage.AttemptRetentionDays = 13
	cfg.Usage.StatsRetentionDays = 14
	cfg.Routing.ObservationRetentionDays = 15

	got := retentionConfigFrom(cfg)
	require.Equal(t, 11, got.LogRetentionDays, "usage_logs 保留天数")
	require.Equal(t, 12, got.ErrLogRetentionDays, "err_logs 保留天数")
	require.Equal(t, 13, got.AttemptRetentionDays,
		"attempt_logs 保留天数（漏接 = 这张表永不退休，且没有任何运行期信号）")
	require.Equal(t, 14, got.StatsRetentionDays, "usage_stats/usage_entity_stats 保留天数")
	require.Equal(t, 15, got.RoutingObservationRetentionDays, "路由观测保留天数")

	// 纯函数：运行时协作者必须留空（它们由装配阶段注入，混进配置投影会让
	// "配置"这个词失去意义）。
	require.Nil(t, got.DiskProbe)
	require.Nil(t, got.OnCapacityBlocked)
	require.Zero(t, got.TickerInterval)
	require.Zero(t, got.DiskMinFreeBytes)
}

// TestRetentionConfigFromDefaultsAreLive 走真实 Load（默认值 + 必填校验）再投影：
// 「默认值写在 config.defaults() 里但没进 projection」与「字段漏接」是同一个终局
// ——两者都让 attempt_logs 永不退休，且没有任何运行期信号。
func TestRetentionConfigFromDefaultsAreLive(t *testing.T) {
	t.Setenv("C3API_AUTH_JWT_SECRET", "test-jwt-secret")
	t.Setenv("C3API_DB_DSN", "postgres://test")
	t.Setenv("C3API_REDIS_ADDR", "127.0.0.1:6379")
	cfg, err := config.Load("")
	require.NoError(t, err)

	got := retentionConfigFrom(cfg)
	require.Equal(t, 7, got.AttemptRetentionDays, "默认 7 天（与 err_logs 同档：同一次失败的两个视角）")
	require.Equal(t, 7, got.ErrLogRetentionDays, "err_logs 默认档位不变")
	require.Greater(t, time.Duration(got.StatsRetentionDays)*24*time.Hour, 24*time.Hour,
		"聚合统计长保留（天数级，不是小时级）")
}
