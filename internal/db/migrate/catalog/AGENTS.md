# internal/db/migrate/catalog — 结构指纹

## 职责

- `Dump` 从 pg_catalog 导出规范化结构：`Static` 参与指纹/相等比较，`Dynamic` 只做
  分区实例的三条合法性校验（YYYYMMDD、父归属、精确 bounds）。
- `PartitionParents` 是分区父表 allowlist；父表的按日分区因此不进入 `Static`。

## 不变量

- `Static` 不含分区实例、不含 `schema_migrations`。
- `PartitionParents` 漏登记 = 分区实例出现在静态指纹（漂移可见）+ adopt 误报；
  新增父表必须同时改这里和 `migrations/`。
- `Violations`（view/trigger/policy/function）在本仓必须为空。

## 验证

```bash
TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/postgres?sslmode=disable \
  go test ./internal/db/migrate/catalog -count=1
```
