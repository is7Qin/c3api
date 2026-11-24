# internal/repository — 持久化门面

## 职责

- 所有 SQL/PG 访问的唯一入口；service/proxy 只拿 Repository 或窄接口。
- 账号身份组查重：`FindAccountExtByCodexKey(..., credType)` 按 `credential.IdentityGroup`
  限定作用域，codex-oauth/codex-pat 同组、BPS 独立；越组即视为不存在。
- 版本化 schema 变更只走 `internal/db/migrate`；运行时的幂等分区/价格 bootstrap DDL
  （`partition.go`、`price_bootstrap.go`）是本目录的既有例外，不 auto-migrate。

## 不变量

- `Repository` 的写方法必须在调用方事务/失效链路内；不得在探测/只读路径调用写方法。
- 事务归属：仓储写方法接收调用方事务（ent tx 或 `pgx.Tx`）；`txDriver` 只服务 ent
  分区路径，不要把「必须走 txDriver」推广到所有仓储。
- `account_exts` 的凭据类型与父模板只需 **BPS 位** 一致：模板是 Responses provider
  （codex/bps）时是硬约束（不一致的账号装载时跳过 + 计数 + ERROR 日志）；静态通道
  模板（api_key/custom/responses-special）下的 ext 行是遗留行，读面不断言、不改行为。
  `ext.CredentialType` 始终是列布局的权威（同通道内 oauth/pat 取不同的凭据列）。
  BPS 行 `upstream_key` 恒为空。
- 分区父表（`partitionedCreateDDL`）的新增必须同步 `catalog.PartitionParents`。

## 验证

```bash
go test ./internal/repository -count=1
```
