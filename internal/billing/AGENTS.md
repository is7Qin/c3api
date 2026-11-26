# internal/billing — 计费核心（纯函数 + 快照 + 游标消费者）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。扣费链路的**端到端**走向见根
> `WHERE TO LOOK` 的"扣费链路"一行；本文件只讲本包的边界与不变量。

## 职责 / 入口

| 文件 | 导出面 |
|---|---|
| `cost.go` | `CostFromResolved` / `CallCostFromResolved` / `ImageCostFromResolved` / `NormalizeTier` / `Tier`（**纯函数**，无状态） |
| `balances.go` | `Balances`（`atomic.Pointer` 整表换的快照，热路径零锁零分配）、`BalanceLoader`（数据源窄接口）、`AssignmentKey`（**别名**，见下）、`NewBalances` |
| `flusher.go` | `Flusher`（批量扣费消费者）、`FlushConfig`、`LedgerStore`（游标消费端口） |
| `drain.go` / `batch_controller.go` | 排空与批次控制 |
| `balance_warning.go` | `BalanceWarningSink`（余额告警，经 `internal/notification`） |
| `stats.go` / `image_usage.go` | 统计与图片用量 |

**扣费落库与请求路径分离**：请求路径只写 `usage_logs` 的出生定态，扣费由本包的游标消费者异步完成。

## 不变量

- **`CostFromResolved` 一族是纯函数**：给定价格快照 + 用量，输出成本。无 DB、无时钟、无全局状态
  ——这是它们能被单测穷举的前提。**不要**在里面读配置或打日志。
- **`Balances` 是只读快照**：`Set` 定向刷新只动余额条目（`*atomic.Int64` 原地 Store），
  **不**整表拷贝换指针。读侧走 atomic，零锁。
- **`billing` 不 import `internal/repository`**：数据源经 `BalanceLoader` / `LedgerStore` 窄接口注入
  （方向固定：实现 → 契约）。这是它能保持无 DB 依赖的原因。
- **`AssignmentKey` 是 `domain.AssignmentKey` 的别名**（`type AssignmentKey = domain.AssignmentKey`）。
  它下沉到 `domain` 是因为**存储层要按这个二元组聚合**，不该为纯二元组反向依赖计费包（P4-c-2）。
  `alias_assert.go` 用**无转换赋值**钉住这个同一性——改名成 defined type 会立刻红。
- **计费扣费路径绝不发布 NOTIFY**（每 flush 即风暴）。见 `internal/notify`。

## 双车道结算

`SettleBalanceBatch` / `SettleFefoBatch` 在 `internal/repository/billing_settle.go`：扣减 + `billed`
标记**同事务原子**（每窗口一次往返）。`runSettleStmt` 的 `marked == batch` 比对守卫 →
`errConcurrentMark`，堵"锁丢失导致双扣"。计数不齐 ⇒ 整事务回滚重放。

## 验证

```bash
go test ./internal/billing/ -count=1
go test ./internal/billing/ -run 'TestEffectiveMultiplier|TestReloadMultipliers' -count=1   # 专属倍率查表
```
⚠ `balances_test.go` 用 `map[AssignmentKey]int{{1, 2}: 5000}` 这类**位置字面量**。`AssignmentKey`
两个字段都是 `int64`，对调后**编译器不报错**，只有这些测试会红——它们是字段顺序的唯一守卫，
不要"顺手重排字段"。

PG 集成测试（`*_pg_test.go`）需要 `TEST_DATABASE_URL` + `TEST_REQUIRE_PG=1`。
