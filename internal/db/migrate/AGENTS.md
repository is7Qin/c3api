# internal/db/migrate — 版本化 schema

## 职责

- `migrations/NNNN_*.sql` 是 schema 唯一来源；runner 按版本执行并记账（checksum、
  `RequiredVersion`、adopt）。
- `gen/` 生成 0001 基线与 head 快照；`catalog/` 导出结构指纹与分区合法性。

## 不变量

- 已应用的 `migrations/*.sql` **不可变**（含注释），漂移会让二进制 NotReady。
- `testdata/0001.catalog.txt` 由 `gen` 重写；`testdata/NNNN.catalog.txt`（N>1）是 head
  快照，只由 `-snapshot-head` 重写，禁止手改。
- 新增运行时分区父表必须登记 `catalog.PartitionParents`，否则真实库 adopt 会把分区
  报成 unexpected，gen 的 `unregisteredPartitionParent` 也会在生成时失败。
- 迁移文件必须 LF、无 BOM。

## 验证

```bash
TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/postgres?sslmode=disable \
  go test ./internal/db/migrate/... -count=1
go run ./internal/db/migrate/gen/cmd -check
go run ./internal/db/migrate/gen/cmd -snapshot-head
```
