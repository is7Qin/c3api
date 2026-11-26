# internal/db/migrate/gen — 迁移 DDL 生成器（离线工具链）

> 根 `AGENTS.md` 覆盖项目级约定；迁移总览见 `internal/db/migrate/AGENTS.md`。

## 职责

`gen.go` / `legacy.go` / `request_patch.go` + `cmd/`：从 ent schema 生成
`0001` 基线 DDL 与 head 快照，并做结构指纹与分区合法性校验。

```bash
go generate ./internal/db/migrate/gen/          # 含 -snapshot-head
go run ./internal/db/migrate/gen/cmd -check     # 校验（CI 用）
go run ./internal/db/migrate/gen/cmd -snapshot-head
```

## 不变量

- **已应用迁移不可变**：schema 变更只**新增** `internal/db/migrate/migrations/NNNN_*`；
  版本水位与 head 快照以迁移包自身为准，**不在别处复写**。
- **`testdata/0001.catalog.txt` 由本工具重写**，不要手改。
- **未注册的分区父表会生成失败**（`unregisteredPartitionParent`）：给表加分区的正确姿势是先在
  catalog 注册，否则 `-check` 与生成都会红。
- **它是离线工具链，不是运行期包**。

## 为什么它在架构账本的 `keep` 区

`internal/db/migrate/gen -> internal/repository` 登记为**结构性不消除**：
生成器**必须**读取 repository 的 baseline DDL 与 hook 排除项（三张分区表）。
这是 `go:generate` 工具链对生成物的依赖，**不是**运行期分层债——不要在清债时误删这条。

## 验证

```bash
go run ./internal/db/migrate/gen/cmd -check
go test ./internal/db/migrate/gen/ -count=1
```
