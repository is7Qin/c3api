# internal/repository — 持久化门面

## 职责

- 所有 SQL/PG 访问的唯一入口；service/proxy 只拿 Repository 或窄接口。
- 账号身份组查重：`FindAccountExtByCodexKey(..., credType)` 按 `credential.IdentityGroup`
  限定作用域，codex-oauth/codex-pat 同组、BPS 独立；越组即视为不存在。
- schema 变更只走 `internal/db/migrate` 的版本化迁移，本目录不执行 DDL、不 auto-migrate。

## 不变量

- `Repository` 的写方法必须在调用方事务/失效链路内；不得在探测/只读路径调用写方法。
- `account_exts` 的凭据类型必须与父模板一致；BPS 行 `upstream_key` 恒为空。
- 分区父表（`partitionedCreateDDL`）的新增必须同步 `catalog.PartitionParents`。

## 验证

```bash
go test ./internal/repository -count=1
```
