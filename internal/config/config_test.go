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
	"gopkg.in/yaml.v3"
)

// setenvRequired 注入必填密钥（auth.jwt_secret/db.dsn/redis.addr 校验已内聚到
// Load——测试调用 Load 前必须先补环境）。
func setenvRequired(t *testing.T) {
	t.Helper()
	t.Setenv("C3API_AUTH_JWT_SECRET", "test-jwt-secret")
	t.Setenv("C3API_DB_DSN", "postgres://test")
	t.Setenv("C3API_REDIS_ADDR", "127.0.0.1:6379")
}

func clearC3APIEnv(t *testing.T) {
	t.Helper()
	var original []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "C3API_") {
			original = append(original, kv)
			require.NoError(t, os.Unsetenv(k))
		}
	}
	t.Cleanup(func() {
		for _, kv := range original {
			if k, v, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(k, v)
			}
		}
	})
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
	require.Equal(t, 8, c.Usage.FlushWorkers, "usage flush 并行 worker 默认 8（管道化）")
	require.Equal(t, 30*time.Second, c.Scheduler.SyncInterval)
	require.True(t, c.Billing.Enabled, "计费默认开（全链默认开启）")
	require.Equal(t, 250*time.Millisecond, c.Billing.FlushInterval, " 游标轮询默认 250ms（spec-f2-ledger-cursor）")
	require.Equal(t, 10*time.Second, c.Billing.BalanceRefreshInterval)
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
	require.Equal(t, 500*time.Millisecond, c.Usage.FlushInterval)
	require.Empty(t, c.Server.TimeZone, "example 的 time_zone 必须位于 server 段")

	deploy, err := Load("../../deploy/config.toml")
	require.NoError(t, err)
	require.Empty(t, deploy.Server.TimeZone, "deploy 模板的 time_zone 必须位于 server 段")
}

// S-E（2026-08-17，2026-10-07 默认翻转）：proxy.behind_cdn 可选键——缺省 = true
// （供应商头识别默认开；旧配置不带此键也走默认 true，加载通过 validate）；显式
// true 加载；显式 false 退回直取 RemoteAddr（零伪造面）。
func TestBehindCDNDefaultTrueAndExplicit(t *testing.T) {
	setenvRequired(t)
	c, err := Load("")
	require.NoError(t, err)
	require.True(t, c.Proxy.BehindCDN, "behind_cdn 缺省 = true（按序采信供应商头）")

	c2, err := Load(writeConfig(t, "[proxy]\nbehind_cdn = true\n"))
	require.NoError(t, err)
	require.True(t, c2.Proxy.BehindCDN, "显式 true 加载")

	c3, err := Load(writeConfig(t, "[proxy]\nbehind_cdn = false\nusage_capture = false\n"))
	require.NoError(t, err)
	require.False(t, c3.Proxy.BehindCDN, "显式 false 退回直取 RemoteAddr")
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

// TestLoadRejectsSubMillisecondDuration：TOML 裸数字（烧穿回归）——500 →
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
// Hook 对 "500" 调 ParseDuration 报错——"missing unit in duration"，附带；
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

// TestLoadFailoverAttemptsRoutingCap1To8：intelligent routing 契约——failover
// 尝试窗口为 1..8（AttemptPlan 固定 [8] attempted-ID 集，>8 无法去重保证）。
// 8 = 最大合法值加载通过；9 / 负值启动即拒绝（错误含字段路径，fail-fast）。
func TestLoadFailoverAttemptsRoutingCap1To8(t *testing.T) {
	setenvRequired(t)
	c, err := Load(writeConfig(t, `proxy = { failover_attempts = 8 }`))
	require.NoError(t, err)
	require.Equal(t, 8, c.Proxy.FailoverAttempts, "8 = 上限合法值")

	for _, v := range []string{"9", "100", "-1"} {
		t.Run("reject "+v, func(t *testing.T) {
			_, err := Load(writeConfig(t, "proxy = { failover_attempts = "+v+" }"))
			require.Error(t, err)
			require.ErrorContains(t, err, "proxy.failover_attempts")
		})
	}
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

// TestLoadRejectsPlaceholderSecrets：3 个已知占位值 × 2 字段（精确匹配拒绝；防
// 原样部署鉴权绕过——config_test 旧断言"change-me 合法"是缺陷守护者，已改写；
// redis.password 复用同校验，foundation spec §2.1）。
func TestLoadRejectsPlaceholderSecrets(t *testing.T) {
	clearC3APIEnv(t)
	for _, ph := range []string{"change-me", "change-me-too", "dev-jwt-secret-for-local"} {
		for _, field := range []string{"auth.jwt_secret", "redis.password"} {
			t.Run(field+"/"+ph, func(t *testing.T) {
				jwt, redisPwd := "real-jwt-secret", ""
				switch field {
				case "auth.jwt_secret":
					jwt = ph
				default:
					redisPwd = ph
				}
				// 不设对应 env（env 会覆盖 TOML 终值）；其余必填经 TOML 提供
				toml := fmt.Sprintf("auth = { jwt_secret = %q }\ndb = { dsn = %q }\nredis = { addr = %q, password = %q }",
					jwt, "postgres://test", "127.0.0.1:6379", redisPwd)
				_, err := Load(writeConfig(t, toml))
				require.Error(t, err)
				require.ErrorContains(t, err, field)
			})
		}
	}
}

// TestLoadRequiresSecrets：必填校验（auth.jwt_secret/db.dsn 空值 → error；原
// main.go:64-66，已内聚到 Load）。
func TestLoadRequiresSecrets(t *testing.T) {
	clearC3APIEnv(t)
	for _, tc := range []struct {
		path string
		toml string
	}{
		{"auth.jwt_secret", "auth = { jwt_secret = \"\" }\ndb = { dsn = \"postgres://test\" }\nredis = { addr = \"127.0.0.1:6379\" }"},
		{"db.dsn", "auth = { jwt_secret = \"s\" }\ndb = { dsn = \"\" }\nredis = { addr = \"127.0.0.1:6379\" }"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.path)
		})
	}
}

// TestLoadRejectsUnknownKeys：ErrorUnused 开启——拼写错误键
// （max_infligh）启动报错，不再静默吞掉。
func TestLoadRejectsUnknownKeys(t *testing.T) {
	setenvRequired(t)
	_, err := Load(writeConfig(t, `proxy = { max_infligh = 1 }`))
	require.Error(t, err)
	require.ErrorContains(t, err, "max_infligh")
}

// TestLoadRejectsRemovedKeys：已移除的键无字段对应 → ErrorUnused 启动报错
// （配置面不向后兼容）。
func TestLoadRejectsRemovedKeys(t *testing.T) {
	setenvRequired(t)
	for _, tc := range []struct {
		name string
		toml string
		key  string
	}{
		{name: "scheduler cooldown", toml: `scheduler = { cooldown_429 = "1s" }`, key: "cooldown_429"},
		{name: "billing workers", toml: `billing = { flush_workers = 8 }`, key: "flush_workers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.key)
		})
	}
}

// 时区：非法 IANA 名 fail-fast，空串通过（进程本地缺省）。
func TestServerTimeZoneValidation(t *testing.T) {
	setenvRequired(t)
	_, err := Load(writeConfig(t, `server = { time_zone = "Not/A_Zone" }`))
	require.Error(t, err)
	require.ErrorContains(t, err, "server.time_zone")
	// 空串 = 进程本地，加载通过
	c, err := Load(writeConfig(t, `server = { time_zone = "" }`))
	require.NoError(t, err)
	require.Equal(t, "", c.Server.TimeZone)
	// 缺省（不写该键）同样通过
	c2, err := Load(writeConfig(t, `proxy = { max_inflight = 7 }`))
	require.NoError(t, err)
	require.Equal(t, "", c2.Server.TimeZone)
}

func TestServerTimeZoneValidIANA(t *testing.T) {
	setenvRequired(t)
	c, err := Load(writeConfig(t, `server = { time_zone = "Asia/Shanghai" }`))
	require.NoError(t, err)
	require.Equal(t, "Asia/Shanghai", c.Server.TimeZone)

	t.Setenv("C3API_SERVER_TIME_ZONE", "Asia/Taipei")
	c, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "Asia/Taipei", c.Server.TimeZone, "env 首下划线映射必须得到 server.time_zone")
}

// TestLoadRedisConfig [redis] 段（spec 2026-08-25-redis-foundation-design §2.1）：
// addr 缺失 fatal（Redis 必选依赖，无"未启用"分支）；env 覆盖三键（首下划线惯例
// C3API_REDIS_ADDR/PASSWORD/DB）；db<0 fatal；db=0 合法；未知键 ErrorUnused fatal。
func TestLoadRedisConfig(t *testing.T) {
	t.Run("addr 缺失 → fatal", func(t *testing.T) {
		setenvRequired(t)
		t.Setenv("C3API_REDIS_ADDR", "") // 显式清空（其余必填已就位，错误唯一归因 redis.addr）
		_, err := Load("")
		require.Error(t, err)
		require.ErrorContains(t, err, "redis.addr")
	})

	t.Run("TOML 解析 + env 覆盖", func(t *testing.T) {
		setenvRequired(t)
		c, err := Load(writeConfig(t, `redis = { addr = "127.0.0.1:6379", password = "", db = 0 }`))
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:6379", c.Redis.Addr)
		require.Equal(t, "", c.Redis.Password)
		require.Equal(t, 0, c.Redis.DB)

		t.Setenv("C3API_REDIS_ADDR", "redis:6379")
		t.Setenv("C3API_REDIS_PASSWORD", "pw")
		t.Setenv("C3API_REDIS_DB", "2")
		c2, err := Load("")
		require.NoError(t, err)
		require.Equal(t, "redis:6379", c2.Redis.Addr, "env 覆盖 TOML/默认")
		require.Equal(t, "pw", c2.Redis.Password)
		require.Equal(t, 2, c2.Redis.DB)
	})

	t.Run("db 负值 → fatal", func(t *testing.T) {
		setenvRequired(t)
		_, err := Load(writeConfig(t, `redis = { addr = "127.0.0.1:6379", db = -1 }`))
		require.Error(t, err)
		require.ErrorContains(t, err, "redis.db")
	})

	t.Run("未知键 → fatal", func(t *testing.T) {
		setenvRequired(t)
		_, err := Load(writeConfig(t, `redis = { addr = "127.0.0.1:6379", tls = true }`))
		require.Error(t, err)
		require.ErrorContains(t, err, "tls")
	})
}

// TestLoadLowBalanceConcDefaults 低余额并发钳制默认 5 / 10（代码默认 + 两份
// toml 显式写出与默认一致）。
func TestLoadLowBalanceConcDefaults(t *testing.T) {
	setenvRequired(t)
	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 5, c.Proxy.LowBalanceMaxConcurrency, "默认 cap = 5")
	require.Equal(t, float64(10), c.Proxy.LowBalanceThresholdUSD, "默认阈值 = $10")

	for _, path := range []string{"../../config.example.toml", "../../deploy/config.toml"} {
		t.Run(path, func(t *testing.T) {
			c, err := Load(path)
			require.NoError(t, err)
			require.Equal(t, 5, c.Proxy.LowBalanceMaxConcurrency)
			require.Equal(t, float64(10), c.Proxy.LowBalanceThresholdUSD)
		})
	}
}

// TestLoadLowBalanceConcValidation 校验规则单一化：cap<0 → 报错；仅 cap>0 时
// 阈值须 >0；cap==0（关闭）→ 两键均不校验（阈值任意值忽略）。
func TestLoadLowBalanceConcValidation(t *testing.T) {
	setenvRequired(t)
	t.Run("cap<0 报错", func(t *testing.T) {
		_, err := Load(writeConfig(t, `proxy = { low_balance_max_concurrency = -1, low_balance_threshold_usd = 10 }`))
		require.Error(t, err)
		require.ErrorContains(t, err, "proxy.low_balance_max_concurrency")
	})
	t.Run("cap>0 且阈值<=0 报错", func(t *testing.T) {
		for _, v := range []string{"0", "-5"} {
			_, err := Load(writeConfig(t, "proxy = { low_balance_max_concurrency = 5, low_balance_threshold_usd = "+v+" }"))
			require.Error(t, err)
			require.ErrorContains(t, err, "proxy.low_balance_threshold_usd")
		}
	})
	t.Run("cap>0 且阈值>0 通过", func(t *testing.T) {
		c, err := Load(writeConfig(t, `proxy = { low_balance_max_concurrency = 5, low_balance_threshold_usd = 2.5 }`))
		require.NoError(t, err)
		require.Equal(t, float64(2.5), c.Proxy.LowBalanceThresholdUSD)
	})
	t.Run("cap==0 时阈值任意值忽略", func(t *testing.T) {
		for _, v := range []string{"0", "-5", "10"} {
			c, err := Load(writeConfig(t, "proxy = { low_balance_max_concurrency = 0, low_balance_threshold_usd = "+v+" }"))
			require.NoError(t, err)
			require.Equal(t, 0, c.Proxy.LowBalanceMaxConcurrency)
		}
	})
}

// TestLoadHTTP2ShardsBounds：upstream.http2_shards 合法域 1..64（默认 1 = 历史行为）。
// 0/负数必须在 shareConns 之前被拒（否则除零/静默单路径）；65 起拒（分片数就是连接
// 数与 fd 的乘数）。
func TestLoadHTTP2ShardsBounds(t *testing.T) {
	setenvRequired(t)

	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 1, c.Upstream.HTTP2Shards, "默认 1：不写该键的部署行为逐字节不变")

	for _, v := range []struct {
		raw  string
		want int
	}{{"1", 1}, {"8", 8}, {"64", 64}} {
		t.Run("accept "+v.raw, func(t *testing.T) {
			c, err := Load(writeConfig(t, "upstream = { http2_shards = "+v.raw+" }"))
			require.NoError(t, err)
			require.Equal(t, v.want, c.Upstream.HTTP2Shards, "加载后的值必须等于输入值")
		})
	}
	for _, v := range []string{"0", "-1", "65"} {
		t.Run("reject "+v, func(t *testing.T) {
			_, err := Load(writeConfig(t, "upstream = { http2_shards = "+v+" }"))
			require.Error(t, err)
			require.ErrorContains(t, err, "upstream.http2_shards")
		})
	}
}

// TestLoadShutdownDrainBounds：server.shutdown_drain 合法域 1ms..10m，默认 2s。
// 0/负值必须启动即拒（Shutdown 会立刻超时 ⇒ 优雅停机名存实亡）；>10m 也拒，因为
// 编排器的 grace period 通常 10s～90s，进程会先被 SIGKILL——配了也不会被执行。
func TestLoadShutdownDrainBounds(t *testing.T) {
	setenvRequired(t)

	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, c.Server.ShutdownDrain, "默认 2s（历史行为）")

	for _, v := range []struct {
		raw  string
		want time.Duration
	}{{"1ms", time.Millisecond}, {"2s", 2 * time.Second}, {"30s", 30 * time.Second}, {"10m", 10 * time.Minute}} {
		t.Run("accept "+v.raw, func(t *testing.T) {
			c, err := Load(writeConfig(t, `server = { shutdown_drain = "`+v.raw+`" }`))
			require.NoError(t, err)
			require.Equal(t, v.want, c.Server.ShutdownDrain, "加载后的值必须等于输入值")
		})
	}
	// 主 deadline 与独立收尾是两件事：预算里恒含 8s 尾部，grace 要求再多 3s。
	require.Equal(t, 8*time.Second, c.Server.ShutdownBudget()-c.Server.ShutdownDrain)
	require.Equal(t, 3*time.Second, c.Server.GracefulStopBudget()-c.Server.ShutdownBudget())

	// env 在 TOML 之后生效（C3API_ 前缀 + 首个下划线转点）。
	t.Run("env 覆盖 TOML", func(t *testing.T) {
		t.Setenv("C3API_SERVER_SHUTDOWN_DRAIN", "45s")
		c, err := Load(writeConfig(t, `server = { shutdown_drain = "2s" }`))
		require.NoError(t, err)
		require.Equal(t, 45*time.Second, c.Server.ShutdownDrain, "env 必须覆盖 TOML")
	})

	for _, v := range []string{"0s", "-1s", "500us", "11m"} {
		t.Run("reject "+v, func(t *testing.T) {
			_, err := Load(writeConfig(t, `server = { shutdown_drain = "`+v+`" }`))
			require.Error(t, err)
			require.ErrorContains(t, err, "server.shutdown_drain")
		})
	}
}

// TestDeployGracePeriodCoversShutdownDrain 是一条**部署契约**门禁：进程看不见编排器
// 给了多少停机时间，只能把仓库里发布的配置当契约读。要覆盖的是"应用**实际**会拿到的
// drain"，三条入口都要算：
//
//	1) 代码默认（不写该键的部署）；
//	2) compose 挂载的 deploy/config.toml；
//	3) helm：config.content（inline 时原样渲染成 TOML）→ 再叠加 app.env 的
//	   C3API_SERVER_SHUTDOWN_DRAIN（env 在文件之后生效，与环境变量优先级一致）。
//
// 三者取最大预算，再要求两份模板的 grace >= **GracefulStopBudget**（预算 + 主 deadline
// 之后仍会跑的独立收尾 3s）。docker 默认的 10s 连主 deadline 都不够，所以必须显式写。
//
// 负例（均已实测红）：compose 删掉 stop_grace_period；helm 的 content 写
// shutdown_drain="60s"；helm 的 env 写 60s；代码默认改成 5s 而模板不动；helm grace 调到 9s。
func TestDeployGracePeriodCoversShutdownDrain(t *testing.T) {
	setenvRequired(t)

	defaults, err := Load("")
	require.NoError(t, err, "代码默认配置必须可加载")
	deployCfg, err := Load("../../deploy/config.toml")
	require.NoError(t, err)

	// --- helm：还原它实际会下发的配置 ---
	helmRaw, err := os.ReadFile("../../deploy/helm/values.yaml")
	require.NoError(t, err)
	var helm struct {
		Config struct {
			Inline  bool   `yaml:"inline"`
			Content string `yaml:"content"`
		} `yaml:"config"`
		App struct {
			TerminationGracePeriodSeconds int               `yaml:"terminationGracePeriodSeconds"`
			Env                           map[string]string `yaml:"env"`
		} `yaml:"app"`
	}
	require.NoError(t, yaml.Unmarshal(helmRaw, &helm))

	helmServer := defaults.Server // 不 inline 时只剩 env + 代码默认
	if helm.Config.Inline {
		path := filepath.Join(t.TempDir(), "helm-config.toml")
		require.NoError(t, os.WriteFile(path, []byte(helm.Config.Content), 0o600))
		hc, err := Load(path)
		require.NoError(t, err, "helm config.content 必须是可加载的 TOML")
		helmServer = hc.Server
	}
	if v := helm.App.Env["C3API_SERVER_SHUTDOWN_DRAIN"]; v != "" {
		d, err := time.ParseDuration(v)
		require.NoError(t, err, "helm app.env 的 C3API_SERVER_SHUTDOWN_DRAIN 必须是合法 duration")
		helmServer.ShutdownDrain = d
	}

	// 三条发布通道取最严的 grace 要求
	need := defaults.Server.GracefulStopBudget()
	for _, b := range []time.Duration{deployCfg.Server.GracefulStopBudget(), helmServer.GracefulStopBudget()} {
		if b > need {
			need = b
		}
	}

	// --- compose：结构化解，不猜缩进 ---
	var compose struct {
		Services map[string]struct {
			StopGracePeriod string `yaml:"stop_grace_period"`
		} `yaml:"services"`
	}
	composeRaw, err := os.ReadFile("../../compose.yml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(composeRaw, &compose))
	appSvc, ok := compose.Services["app"]
	require.True(t, ok, "compose.yml 必须有 app 服务")
	composeGrace, err := time.ParseDuration(appSvc.StopGracePeriod)
	require.NoError(t, err, "compose 的 app 服务必须显式给 stop_grace_period（docker 默认 10s 连主 deadline 都不够）")
	require.GreaterOrEqual(t, composeGrace, need,
		"compose stop_grace_period(%s) 必须 >= 所需 grace(%s)：否则进程在收尾途中被 SIGKILL", composeGrace, need)

	// --- helm：grace 必须覆盖同一条链 ---
	helmGrace := time.Duration(helm.App.TerminationGracePeriodSeconds) * time.Second
	require.GreaterOrEqual(t, helmGrace, need,
		"helm terminationGracePeriodSeconds(%s) 必须 >= 所需 grace(%s)", helmGrace, need)
}
