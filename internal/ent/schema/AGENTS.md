# internal/ent/schema — ent 实体定义（**生成源**，不是生成物）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/ent` 其余目录是**生成代码，禁止手改**。

## 这里是唯一该改的地方

改 ent 模型的正确姿势：**改本目录的 schema → 重生成**，绝不手改 `internal/ent/*.go`。

```bash
go generate ./internal/ent/     # 内部跑 `go run -mod=mod entgo.io/ent/cmd/ent generate --feature sql/upsert --target . ./schema`
git checkout -- go.sum          # ⚠ -mod=mod 会污染 go.sum（实测加 30 行，go.mod 不变）
```

⚠ **`go generate` 会在干净树上也加 30 行 `go.sum`**（`clipperhouse/*`、`fatih/color` 等 entc 传递
依赖）。必须 `git checkout -- go.sum` 还原，最终 diff 里 `go.sum` 必须零改动。

## 已登记的架构债

`ent/schema → internal/domain`（`productionLedger`）：4 组 status 枚举直接用 `domain` 常量
（`UserStatusActive` / `KeyStatusActive` / `ManagementKeyStatusActive` / `RedemptionStatusActive`）。
状态枚举下沉是**另一类问题**（牵涉 `status_literal_gate_test.go` 的四张表与 `field.Enum(...).Values(...)`
的 DB enum 生成），需单独一单。

两个 JSON 列已下沉到中立叶子包（R2-a）：
- `template.go` 的 `field.JSON("model_mapping", modelmap.ModelMapping{})`
- `account_ext.go` 的 `field.JSON("codex_identity", &identityenv.IdentityEnvelope{})`

## 不变量

- **三张表被 migrate hook 排除**：`usage_logs` / `err_logs` / `usage_stats`（日分区 DDL 归
  `internal/usage` 的 `PartitionManager`）。
- **`ttft_hist` 不进 schema**：PG `bigint[]`，ent 无数组类型（`field.Ints` 是 JSON 语义）。
- **改 schema 可能要求新迁移**：schema 变更只新增 `internal/db/migrate/migrations/NNNN_*`，
  已应用迁移**不可变**。
- **枚举值来自单一事实源**：`field.Enum(...).Values(string(domain.XxxActive), …)`——不要写字面量。

## 验证

```bash
go generate ./internal/ent/ && git checkout -- go.sum
go build ./internal/ent/ && echo OK
go test ./internal/ent/schema/ -count=1        # status_literal_gate_test 等
go test ./internal/architecture/ -count=1      # 确认 ent 的依赖边没被动坏
```
