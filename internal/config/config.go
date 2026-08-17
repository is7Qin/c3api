// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"encoding"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	Server    ServerConfig    `koanf:"server"`
	Log       LogConfig       `koanf:"log"`
	Admin     AdminConfig     `koanf:"admin"`
	Auth      AuthConfig      `koanf:"auth"`
	DB        DBConfig        `koanf:"db"`
	Proxy     ProxyConfig     `koanf:"proxy"`
	Upstream  UpstreamConfig  `koanf:"upstream"`
	Limit     LimitConfig     `koanf:"limit"`
	Scheduler SchedulerConfig `koanf:"scheduler"`
	Usage     UsageConfig     `koanf:"usage"`
	Billing   BillingConfig   `koanf:"billing"`
}

type ServerConfig struct {
	Addr              string        `koanf:"addr"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	MaxHeaderBytes    int           `koanf:"max_header_bytes"`
}

type LogConfig struct {
	Level  string `koanf:"level"`
	Output string `koanf:"output"`
}

type AdminConfig struct {
	Token string `koanf:"token"`
}

// AuthConfig JWT 密钥：强制（C3API_AUTH_JWT_SECRET），缺失启动失败——
// 随机生成 = 重启全失效 + 多实例不一致（评审定夺①）。
type AuthConfig struct {
	JWTSecret string `koanf:"jwt_secret"`
}

// DBConfig 数据库连接。DSN 无需手工写 lock_timeout——OpenPG 统一补丁
// （F-P2-4：计费路径防卡死 lock_timeout=5s 会话级 + 计费扣费事务 per-query
// 10s 超时 + MaxConnLifetime=30m 滚动轮换，详见 repository.OpenPG /
// BillingRepo.DeductAndLog；statement_timeout 不设会话级——与 admin 面
// ScanStats 大窗口聚合实测冲突降级，见 f1-impl-report.md；用户 DSN 已显式
// 配置同名参数时尊重用户配置不覆盖）。
type DBConfig struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

type ProxyConfig struct {
	MaxBodySize           int64         `koanf:"max_body_size"`
	MaxInflight           int64         `koanf:"max_inflight"`
	UpstreamTimeout       time.Duration `koanf:"upstream_timeout"`
	UpstreamStreamTimeout time.Duration `koanf:"upstream_stream_timeout"`
	// FailoverAttempts 总尝试次数（含首次）——>= 1（0 启动即拒绝：failover 循环
	// 零次执行，首次选号占用的并发槽永不释放，组内账号耗尽后全组 429 死锁）。
	FailoverAttempts      int           `koanf:"failover_attempts"`
	UsageCapture          bool          `koanf:"usage_capture"`
	// BehindCDN 客户端 IP 识别开关（用户裁决 2026-08-17：config 文件键，非 admin
	// setting）：false（默认）→ 完全不读供应商头（CF-Connecting-IP /
	// True-Client-IP / X-Real-IP），直取 RemoteAddr（零伪造面，与直连行为一致）；
	// true → 按序采信三头。部署前提：源站只对 CDN/反向代理暴露（防火墙层封
	// 直连）——直连时可自填任意值，client_ip 为审计/排障的尽力而为标识，非安全
	// 边界。可选键，旧配置不带此键照常加载（零值 false）。
	BehindCDN bool `koanf:"behind_cdn"`
}

type UpstreamConfig struct {
	MaxIdleConns        int           `koanf:"max_idle_conns"`
	MaxIdleConnsPerHost int           `koanf:"max_idle_conns_per_host"`
	IdleConnTimeout     time.Duration `koanf:"idle_conn_timeout"`
	DialTimeout         time.Duration `koanf:"dial_timeout"`
	ForceHTTP2          bool          `koanf:"force_http2"`
}

type LimitConfig struct {
	GroupKeyRPM int `koanf:"group_key_rpm"`
}

// SchedulerConfig 注意：cooldown_429 / backoff_base / backoff_max 已删除
// （不向后兼容）——配置含这些旧键将启动报错（ErrorUnused）。
type SchedulerConfig struct {
	DefaultMaxConcurrency int           `koanf:"default_max_concurrency"`
	SyncInterval          time.Duration `koanf:"sync_interval"`
}

type UsageConfig struct {
	BatchSize          int           `koanf:"batch_size"`
	FlushInterval      time.Duration `koanf:"flush_interval"`
	LogRetentionDays   int           `koanf:"log_retention_days"`
	QuotaFlushInterval time.Duration `koanf:"quota_flush_interval"` // quota 增量批量回写 cadence
	FlushWorkers       int           `koanf:"flush_workers"`        // flush 并行 worker 数（O1 管道化分片并行；明细/额度共用）
	// StatsAggInterval 离线聚合周期（spec 2026-08-14：使用量统计离线聚合化——
	// 独立 worker 每周期从 DB 重建 usage_stats；默认 5m；0 = 禁用聚合）。
	StatsAggInterval time.Duration `koanf:"stats_agg_interval"`
	// err_logs 错误审计明细（分表设计）：有界队列 + 背压采样丢弃（风暴不淹没
	// DB 不爆内存；DB 写速率上界 = ErrLogBatchSize/ErrLogFlushInterval）。
	ErrLogQueueSize     int           `koanf:"errlog_queue_size"`     // 队列容量（默认 4096）
	ErrLogBatchSize     int           `koanf:"errlog_batch_size"`     // 每批落盘行数（默认 500）
	ErrLogFlushInterval time.Duration `koanf:"errlog_flush_interval"` // 批间隔（默认 500ms）
	ErrLogRetentionDays int           `koanf:"errlog_retention_days"` // err_logs 分区保留天数（默认 7 天短保留——错误审计；<= 0 = 不删除）
	// StatsRetentionDays usage_stats 分区保留天数（默认 180 天——聚合统计长保留；
	// usage_stats 也分区化，清理 DROP 分区 O(1)——PG DELETE 不释放空间；
	// <= 0 = 不删除）。
	StatsRetentionDays int `koanf:"stats_retention_days"`
}

// BillingConfig 计费（Phase 5 T3）：Enabled 默认开（全链默认开启：代码默认 +
// 模板默认一致；空价格表 = 全模型 402——首次启动需先同步价格（POST
// /admin/pricing/sync）；余额预检 + FEFO 条件扣费 + 优雅停机排空全链随之生效。
// 本地开发可用 enabled=false 显式退回纯代理模式）。
type BillingConfig struct {
	Enabled                bool          `koanf:"enabled"`
	FlushInterval          time.Duration `koanf:"flush_interval"`           // 扣费落库周期
	BalanceRefreshInterval time.Duration `koanf:"balance_refresh_interval"` // 余额快照全量刷新周期
	FlushWorkers           int           `koanf:"flush_workers"`            // flush 并行 worker 数（O1 管道化分片并行）
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{Addr: ":8080", ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20},
		Log:    LogConfig{Level: "warn", Output: "stdout"},
		// #17：10→20（billing 8 worker + stats 8 worker + 余量；统计 COPY 批量写已改毫秒级短事务）。
		// 连接参数（lock_timeout=5s 会话级 + 计费 per-query 10s 超时 + MaxConnLifetime=30m，
		// F-P2-4 计费路径防卡死）由 OpenPG/DeductAndLog 统一补，DSN 无需手工写（用户显式
		// 配置同名参数时尊重不覆盖；statement_timeout 不设会话级——副作用核实见 f1-impl-report.md）。
		DB:        DBConfig{MaxConns: 20},
		Proxy:     ProxyConfig{MaxBodySize: 4 << 20, MaxInflight: 50000, UpstreamTimeout: 120 * time.Second, UpstreamStreamTimeout: 30 * time.Minute, FailoverAttempts: 3, UsageCapture: true},
		Upstream:  UpstreamConfig{MaxIdleConns: 8192, MaxIdleConnsPerHost: 2048, IdleConnTimeout: 90 * time.Second, DialTimeout: 10 * time.Second, ForceHTTP2: true},
		Scheduler: SchedulerConfig{DefaultMaxConcurrency: 8, SyncInterval: 30 * time.Second},
		Usage:     UsageConfig{BatchSize: 500, FlushInterval: 500 * time.Millisecond, LogRetentionDays: 30, QuotaFlushInterval: 10 * time.Second, FlushWorkers: 8, StatsAggInterval: 5 * time.Minute, ErrLogQueueSize: 4096, ErrLogBatchSize: 500, ErrLogFlushInterval: 500 * time.Millisecond, ErrLogRetentionDays: 7, StatsRetentionDays: 180},
		Billing:   BillingConfig{Enabled: true, FlushInterval: 1 * time.Second, BalanceRefreshInterval: 10 * time.Second, FlushWorkers: 8},
	}
}

// Load 先应用默认值，再叠加 TOML 文件，最后叠加 C3API_ 前缀 env（前缀必须大写），
// 然后统一校验（validate：duration/数值下限、必填、占位密钥、未知键——fail-fast）。
// 配置仅启动时读取，变更需滚动重启（无热更新）。
func Load(path string) (*Config, error) {
	c := defaults()
	k := koanf.New(".")
	if path != "" {
		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return nil, err
		}
	}
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: "C3API_",
		TransformFunc: func(k, v string) (string, any) {
			k = strings.ToLower(strings.TrimPrefix(k, "C3API_"))
			if i := strings.Index(k, "_"); i >= 0 {
				k = k[:i] + "." + k[i+1:]
			}
			return k, v
		},
	}), nil); err != nil {
		return nil, err
	}
	// ErrorUnused：配置显式写未知键（拼写错误/已删旧键）→ 启动报错（D-P2-1）。
	// ⚠ DecoderConfig 必须完整复制 koanf 默认（StringToTimeDurationHookFunc +
	// textUnmarshalerHookFunc + WeaklyTypedInput: true）——漏任一：duration 字符串
	// 解析（"500ms"）全失效，且 env 路径裸数字 fail-fast 保护丢失（p2-14 P2-A 交叉风险）。
	if err := k.UnmarshalWithConf("", c, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				textUnmarshalerHookFunc()),
			WeaklyTypedInput: true,
			ErrorUnused:      true,
		},
	}); err != nil {
		return nil, err
	}
	return c, validate(c)
}

// validate Load 末尾统一校验（fail-fast，错误含 koanf 字段路径，形如
// "scheduler.sync_interval must be > 0"）：
//   - duration 字段 ≥1ms 硬校验：拦截 5 处 time.NewTicker panic 面（scheduler/
//     usage×2/flusher×2）与 errlog.go:123 第 6 个 ticker 的 500ns 烧穿面（D-P2-2：
//     裸数字 `flush_interval = 500` → 500ns ticker → 队列非空 DB 写风暴 / 队列空
//     CPU 忙轮询）。errlog_flush_interval=0 由"钳位到默认"变为"启动报错"——有意
//     选择（errlog 无文档化"0=禁用"语义，取 p2-14"全部 duration 字段"立场）；
//   - 数值字段 ≥1：DefaultMaxConcurrency（silent 全坏面——从"健康地拒绝全流量"
//     转启动即报错）、DB.MaxConns（puddle 层报 MaxSize 无法归因到 db.max_conns）；
//   - 必填：auth.jwt_secret / db.dsn（自 main.go:64-66 移入内聚；admin.token
//     已可空——空 = 不启用静态 token 鉴权，/admin 仅接受 platform_admin JWT）；
//   - 占位密钥精确匹配拒绝（change-me 系列防原样部署鉴权绕过；精确匹配防误杀恰
//     以 change-me 开头的合法随机值，派生占位由"空值 + 强制 env"形态兜底）。
//
// 明确排除：retention 天数（<=0 = 不删除，文档化惯例）、int 型钳位字段
// （BatchSize/FlushWorkers/ErrLogQueueSize/ErrLogBatchSize）、Limit.GroupKeyRPM
// （出厂 0）、Proxy.MaxInflight（server 侧 0→50000 兜底，proxy 消费语义未核实——
// 范围外）。
func validate(c *Config) error {
	for _, d := range []struct {
		path      string
		value     time.Duration
		allowZero bool // 0 = 合法语义（禁用以外的 duration 字段）
	}{
		{"proxy.upstream_timeout", c.Proxy.UpstreamTimeout, false},
		{"proxy.upstream_stream_timeout", c.Proxy.UpstreamStreamTimeout, false},
		{"scheduler.sync_interval", c.Scheduler.SyncInterval, false},
		{"usage.flush_interval", c.Usage.FlushInterval, false},
		{"usage.quota_flush_interval", c.Usage.QuotaFlushInterval, false},
		{"usage.errlog_flush_interval", c.Usage.ErrLogFlushInterval, false},
		// stats_agg_interval：0 = 禁用聚合（合法语义）；非 0 必须 ≥1ms（防 ticker
		// panic 面——裸数字 500 → 500ns 合法值域外）。
		{"usage.stats_agg_interval", c.Usage.StatsAggInterval, true},
		{"billing.flush_interval", c.Billing.FlushInterval, false},
		{"billing.balance_refresh_interval", c.Billing.BalanceRefreshInterval, false},
		// upstream 连接池超时：0 = 永不回收 idle 连接 / 无拨号超时（与 fail-fast
		// 哲学相悖——spec 2026-08-17 补下限；默认 90s/10s 安全不受影响）。
		{"upstream.idle_conn_timeout", c.Upstream.IdleConnTimeout, false},
		{"upstream.dial_timeout", c.Upstream.DialTimeout, false},
	} {
		if d.allowZero && d.value == 0 {
			continue
		}
		if d.value < time.Millisecond {
			return fmt.Errorf("%s must be >= 1ms (got %s)", d.path, d.value)
		}
	}
	// int 下限表——proxy.failover_attempts 语义为"总尝试次数（含首次）"（total
	// attempts, first attempt counts）：0 会绕过 failover 循环且首次选号占用的
	// 并发槽永不释放（组内账号耗尽后全组 429 死锁），启动即拒绝。
	for _, n := range []struct {
		path  string
		value int
	}{
		{"scheduler.default_max_concurrency", c.Scheduler.DefaultMaxConcurrency},
		{"db.max_conns", c.DB.MaxConns},
		{"proxy.failover_attempts", c.Proxy.FailoverAttempts},
		// proxy.max_body_size：n<1 经 MaxBytesReader 归一 0 → 全量非空请求 413
		// （internal/proxy/caller.go 用法；spec 2026-08-17 补下限，启动即拒绝）。
		{"proxy.max_body_size", int(c.Proxy.MaxBodySize)},
	} {
		if n.value < 1 {
			return fmt.Errorf("%s must be >= 1 (got %d)", n.path, n.value)
		}
	}
	for _, r := range []struct {
		path  string
		value string
	}{
		{"auth.jwt_secret", c.Auth.JWTSecret},
		{"db.dsn", c.DB.DSN},
	} {
		if r.value == "" {
			return fmt.Errorf("%s is required (set in config file or C3API_AUTH_JWT_SECRET/C3API_DB_DSN)", r.path)
		}
	}
	for _, p := range []struct {
		path  string
		value string
	}{
		{"admin.token", c.Admin.Token},
		{"auth.jwt_secret", c.Auth.JWTSecret},
	} {
		switch p.value {
		case "change-me", "change-me-too", "dev-admin-token", "dev-jwt-secret-for-local":
			return fmt.Errorf("%s must not be a placeholder value (got %q); inject via C3API_ADMIN_TOKEN/C3API_AUTH_JWT_SECRET", p.path, p.value)
		}
	}
	return nil
}

// textUnmarshalerHookFunc 镜像 koanf v2.3.6 内部同名 hook（未导出，spec 要求完整
// 复制默认 DecoderConfig）：支持实现 encoding.TextUnmarshaler 的自定义 string 类型。
// 现配置结构无此类字段，保留与 koanf 默认行为逐位一致（防后续字段类型变更漂移）。
func textUnmarshalerHookFunc() mapstructure.DecodeHookFuncType {
	return func(
		f reflect.Type,
		t reflect.Type,
		data any,
	) (any, error) {
		if f.Kind() != reflect.String {
			return data, nil
		}
		result := reflect.New(t).Interface()
		unmarshaller, ok := result.(encoding.TextUnmarshaler)
		if !ok {
			return data, nil
		}

		// default text representation is the actual value of the `from` string
		var (
			dataVal = reflect.ValueOf(data)
			text    = []byte(dataVal.String())
		)
		if f.Kind() == t.Kind() {
			// source and target are of underlying type string
			var (
				err    error
				ptrVal = reflect.New(dataVal.Type())
			)
			if !ptrVal.Elem().CanSet() {
				// cannot set, skip, this should not happen
				if err := unmarshaller.UnmarshalText(text); err != nil {
					return nil, err
				}
				return result, nil
			}
			ptrVal.Elem().Set(dataVal)

			// We need to assert that both, the value type and the pointer type
			// do (not) implement the TextMarshaller interface before proceeding and simply
			// using the string value of the string type.
			// it might be the case that the internal string representation differs from
			// the (un)marshalled string.

			for _, v := range []reflect.Value{dataVal, ptrVal} {
				if marshaller, ok := v.Interface().(encoding.TextMarshaler); ok {
					text, err = marshaller.MarshalText()
					if err != nil {
						return nil, err
					}
					break
				}
			}
		}

		// text is either the source string's value or the source string type's marshaled value
		// which may differ from its internal string value.
		if err := unmarshaller.UnmarshalText(text); err != nil {
			return nil, err
		}
		return result, nil
	}
}
