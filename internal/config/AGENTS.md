# internal/config — 配置加载与 fail-fast 校验

> 根 `AGENTS.md` 覆盖项目级约定。配置项清单以 `config.example.toml` + 本包为准。

## 职责

`config.go` 的 `Load` 把 TOML 解析成 `Config`（分 `AuthConfig` / `BillingConfig` / `DBConfig` /
`LogConfig` / `ProxyConfig` / `RedisConfig` / `RoutingConfig` / `SchedulerConfig` / `ServerConfig` /
`UpstreamConfig` / `UsageConfig`），`supplier.go` 是供应商面配置。

## 不变量（fail-fast，启动即炸）

- **未知键 = 启动失败**（不做静默忽略）。
- **时长 < 1ms = 失败**；**受控整数 < 1 = 失败**。
- **占位密钥**（`change-me` 之类）= 失败。
- **改 TOML 键名 = 故意的破坏性变更**，必须同步 `config.example.toml` 与文档。
- `EffectiveMaxInflight` 是派生值（预算分摊），不要在别处复写第二份算法。

## 验证

```bash
go test ./internal/config/ -count=1
```
