// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// setenvRequired 注入必填两密钥（auth.jwt_secret/db.dsn 校验已内聚到 Load——
// 测试调用 Load 前必须先补环境，评审 P2-1；admin.token 已可空，仍注入保持
// 既有用例语义）。
func setenvRequired(t *testing.T) {
	t.Helper()
	t.Setenv("C3API_ADMIN_TOKEN", "test-admin-token")
	t.Setenv("C3API_AUTH_JWT_SECRET", "test-jwt-secret")
	t.Setenv("C3API_DB_DSN", "postgres://test")
}

// writeConfig 写临时 TOML 并返回路径（单键覆盖 + 默认值叠加场景）。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestDefaults(t *testing.T) {
	setenvRequired(t)
	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "warn", c.Log.Level)
	require.Equal(t, int64(50000), c.Proxy.MaxInflight)
	require.Equal(t, 500, c.Usage.BatchSize)
	require.Equal(t, 8, c.Usage.FlushWorkers, "usage flush 并行 worker 默认 8（O1 管道化）")
	require.Equal(t, "test-admin-token", c.Admin.Token)
	require.Equal(t, 30*time.Second, c.Scheduler.SyncInterval)
	require.True(t, c.Billing.Enabled, "计费默认开（全链默认开启）")
	require.Equal(t, 250*time.Millisecond, c.Billing.FlushInterval, "F2 游标轮询默认 250ms（spec-f2-ledger-cursor）")
	require.Equal(t, 10*time.Second, c.Billing.BalanceRefreshInterval)
	require.Equal(t, 8, c.Billing.FlushWorkers, "flush 并行 worker 默认 8（O1）")
}

// TestEnvOverlay 改造后保持通过：env 叠加 + duration 解析回归（原只设
// MAX_INFLIGHT，必填校验移入 Load 后需补三密钥）。
func TestEnvOverlay(t *testing.T) {
	setenvRequired(t)
	t.Setenv("C3API_PROXY_MAX_INFLIGHT", "7")
	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, int64(7), c.Proxy.MaxInflight)
}

// TestLoadFromTOML 非占位正常路径：example 占位值已改空 + 强制 env 注入——经 env
// 注入三密钥验证文件加载与 duration 字符串解析（"500ms"）回归。
func TestLoadFromTOML(t *testing.T) {
	setenvRequired(t)
	c, err := Load("../../config.example.toml")
	require.NoError(t, err)
	require.Equal(t, "test-admin-token", c.Admin.Token)
	require.Equal(t, 500*time.Millisecond, c.Usage.FlushInterval)
}

// S-E（2026-08-17）：proxy.behind_cdn 可选键——缺省 = false（零伪造面默认；
// 旧配置不带此键照常加载通过 validate）；显式 true 加载；显式 false 等价缺省。
func TestBehindCDNDefaultFalseAndExplicit(t *testing.T) {
	setenvRequired(t)
	c, err := Load("")
	require.NoError(t, err)
	require.False(t, c.Proxy.BehindCDN, "behind_cdn 缺省 = false（完全不读供应商头）")

	c2, err := Load(writeConfig(t, "[proxy]\nbehind_cdn = true\n"))
	require.NoError(t, err)
	require.True(t, c2.Proxy.BehindCDN, "显式 true 加载")

	c3, err := Load(writeConfig(t, "[proxy]\nbehind_cdn = false\nusage_capture = false\n"))
	require.NoError(t, err)
	require.False(t, c3.Proxy.BehindCDN, "显式 false 等价缺省")
}

// TestLoadRejectsNonPositiveDurations：8 个 duration 字段 × 0/-1s → error 含字段名
// （5 处 ticker panic 面 + errlog.go:123 第 6 个 ticker 烧穿面）。
func TestLoadRejectsNonPositiveDurations(t *testing.T) {
	setenvRequired(t)
	for _, tc := range []struct {
		path string // koanf 字段路径（错误消息断言）
	}{
		{"proxy.upstream_timeout"},
		{"proxy.upstream_stream_timeout"},
		{"scheduler.sync_interval"},
		{"usage.flush_interval"},
		{"usage.quota_flush_interval"},
		{"usage.errlog_flush_interval"},
		{"billing.flush_interval"},
		{"billing.balance_refresh_interval"},
		{"upstream.idle_conn_timeout"},
		{"upstream.dial_timeout"},
	} {
		sec, key, _ := strings.Cut(tc.path, ".")
		for _, v := range []string{"0s", "-1s"} {
			t.Run(tc.path+"/"+v, func(t *testing.T) {
				_, err := Load(writeConfig(t, fmt.Sprintf("%s = { %s = %q }", sec, key, v)))
				require.Error(t, err)
				require.ErrorContains(t, err, tc.path)
			})
		}
	}
}

// TestLoadRejectsSubMillisecondDuration：TOML 裸数字（D-P2-2 烧穿回归）——500 →
// 500ns（<1ms）被 ≥1ms 校验拦截（errlog_flush_interval 无 0=禁用 语义，
// 钳位仅覆盖 <=0，500ns > 0 不被钳）。
func TestLoadRejectsSubMillisecondDuration(t *testing.T) {
	setenvRequired(t)
	for _, tc := range []struct {
		path string
		toml string
	}{
		{"usage.flush_interval", `usage = { flush_interval = 500 }`},
		{"usage.errlog_flush_interval", `usage = { errlog_flush_interval = 500 }`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.path)
		})
	}
}

// TestLoadEnvBareNumberFailFast：env 路径裸数字天然 fail-fast（StringToTimeDuration
// Hook 对 "500" 调 ParseDuration 报错——"missing unit in duration"，D-P2-2 附带；
// 文件路径同值由 ≥1ms 校验拦截）——锁行为防 ErrorUnused/WeaklyTyped 改造误破坏。
func TestLoadEnvBareNumberFailFast(t *testing.T) {
	setenvRequired(t)
	t.Setenv("C3API_USAGE_FLUSH_INTERVAL", "500")
	_, err := Load("")
	require.Error(t, err)
	require.ErrorContains(t, err, "missing unit in duration")
}

// TestLoadRejectsNonPositiveNumeric：数值字段下限 ≥1——DefaultMaxConcurrency=0
// （silent 全坏面：从"健康地拒绝全流量"转启动即报错）、DB.MaxConns=0（puddle 层
// MaxSize 报错无法归因到 db.max_conns）、FailoverAttempts=0（failover 循环零次
// 执行 → 首次选号并发槽永不释放，死锁配置启动即拒绝）。
func TestLoadRejectsNonPositiveNumeric(t *testing.T) {
	setenvRequired(t)
	for _, tc := range []struct {
		path string
		toml string
	}{
		{"scheduler.default_max_concurrency", `scheduler = { default_max_concurrency = 0 }`},
		{"db.max_conns", `db = { max_conns = 0 }`},
		{"proxy.failover_attempts", `proxy = { failover_attempts = 0 }`},
		// proxy.max_body_size：n<1 经 MaxBytesReader 归一 0 → 全量非空请求 413，
		// 启动即拒绝（spec 2026-08-17 补下限）。
		{"proxy.max_body_size", `proxy = { max_body_size = 0 }`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.path)
		})
	}
}

// TestLoadAcceptsFailoverAttemptsOne：failover_attempts=1（最小合法值，单次尝试
// 不重试）加载通过——与 0 拒绝成对锁定下限语义。
func TestLoadAcceptsFailoverAttemptsOne(t *testing.T) {
	setenvRequired(t)
	c, err := Load(writeConfig(t, `proxy = { failover_attempts = 1 }`))
	require.NoError(t, err)
	require.Equal(t, 1, c.Proxy.FailoverAttempts)
}

// TestLoadKeepsRetentionZeroSemantics：retention 天数 <=0 = 不删除（文档化惯例，
// 不得改为报错）。
func TestLoadKeepsRetentionZeroSemantics(t *testing.T) {
	setenvRequired(t)
	for _, toml := range []string{
		`usage = { log_retention_days = 0 }`,
		`usage = { errlog_retention_days = 0 }`,
		`usage = { stats_retention_days = 0 }`,
	} {
		_, err := Load(writeConfig(t, toml))
		require.NoError(t, err)
	}
}

// TestLoadRejectsPlaceholderSecrets：4 个已知占位值 × 2 字段（精确匹配拒绝；防
// 原样部署鉴权绕过——config_test 旧断言"change-me 合法"是缺陷守护者，已改写）。
func TestLoadRejectsPlaceholderSecrets(t *testing.T) {
	for _, ph := range []string{"change-me", "change-me-too", "dev-admin-token", "dev-jwt-secret-for-local"} {
		for _, field := range []string{"admin.token", "auth.jwt_secret"} {
			t.Run(field+"/"+ph, func(t *testing.T) {
				admin, jwt := "real-admin-token", "real-jwt-secret"
				if field == "admin.token" {
					admin = ph
				} else {
					jwt = ph
				}
				// 不设对应 env（env 会覆盖 TOML 终值）；其余必填经 TOML 提供
				toml := fmt.Sprintf("admin = { token = %q }\nauth = { jwt_secret = %q }\ndb = { dsn = %q }", admin, jwt, "postgres://test")
				_, err := Load(writeConfig(t, toml))
				require.Error(t, err)
				require.ErrorContains(t, err, field)
			})
		}
	}
}

// TestLoadRequiresSecrets：必填校验（auth.jwt_secret/db.dsn 空值 → error；原
// main.go:64-66，已内聚到 Load）。admin.token 已可空——空 = 不启用静态 token，
// 不再报错（spec 2026-08-15）。
func TestLoadRequiresSecrets(t *testing.T) {
	for _, tc := range []struct {
		path string
		toml string
	}{
		{"auth.jwt_secret", "admin = { token = \"\" }\nauth = { jwt_secret = \"\" }\ndb = { dsn = \"postgres://test\" }"},
		{"db.dsn", "admin = { token = \"\" }\nauth = { jwt_secret = \"s\" }\ndb = { dsn = \"\" }"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.path)
		})
	}
	// admin.token 空 + 其余必填齐 → 启动成功（空 = 不启用静态 token）
	_, err := Load(writeConfig(t, "admin = { token = \"\" }\nauth = { jwt_secret = \"s\" }\ndb = { dsn = \"postgres://test\" }"))
	require.NoError(t, err)
}

// TestLoadRejectsUnknownKeys：ErrorUnused 开启（D-P2-1）——拼写错误键
// （max_infligh）启动报错，不再静默吞掉。
func TestLoadRejectsUnknownKeys(t *testing.T) {
	setenvRequired(t)
	_, err := Load(writeConfig(t, `proxy = { max_infligh = 1 }`))
	require.Error(t, err)
	require.ErrorContains(t, err, "max_infligh")
}

// TestLoadRejectsLegacyKeys：废弃字段（Cooldown429/BackoffBase/BackoffMax）已
// 删除（不向后兼容）——显式写旧键 → ErrorUnused 启动报错。
func TestLoadRejectsLegacyKeys(t *testing.T) {
	setenvRequired(t)
	_, err := Load(writeConfig(t, `scheduler = { cooldown_429 = "1s" }`))
	require.Error(t, err)
	require.ErrorContains(t, err, "cooldown_429")
}
