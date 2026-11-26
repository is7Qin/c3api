# internal/rule — 规则引擎（事件 → worker → typed 动作）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

事件 → **有界 channel** → 规则 worker（priority 首中匹配）→ typed 动作 + 响应整形。

| 文件 | 内容 |
|---|---|
| `engine.go` | `RuleEngine` / `Event` / `Kind` / `HealthSink` / `PersistFunc` / `Config` / `New` |
| `evaluate.go` | `Match`（when 判据求值） |
| `validate.go` | `ValidateThen` / `ValidateWhen`（管理面写入前置校验） |
| `windows.go` | 滑动窗口计数（429 / 失败 / ok / total 与 ratio 族） |
| `persist_error.go` | `ClassifyPersistError` / `PersistErrorKind`（落库错误分类） |
| `set.go` / `stats.go` / `worker.go` | 规则集合、统计、worker 循环 |

`Kind` 六值：`ok` / `429` / `4xx` / `5xx` / `network` / `timeout`。

⚠ **`Kind` 是 `domain.RuleKind` 的别名（`type Kind = domain.RuleKind`），不是本包
定义的类型**：canonical 定义、数值、顺序、`String()` 全在 `internal/domain/rulekind.go`，
本包只提供常量别名（`KindOK = domain.RuleKindOK` 等，6 个全部显式赋值）。改值/加值
一律改 domain 那一份——在这里重新赋值就是造第二套枚举（历史上 `KindTimeout` 靠
iota 续行、与 `KindNetwork` 撞成同值 4，`String()` 直接编译失败报 duplicate case）。
别名方案的目的：用户面 handler 要用同一批类别却不必 import 本包，而 proxy /
scheduler 等既有 `rule.Kind*` 引用点零改动。

## 不变量

- **`scheduler.MarkResult` 的硬编码状态机由本包替代**：scheduler 只做禁用守卫 + 事件投递
  （条件投递），持久状态列已随 cutover 删除。别把状态机加回 scheduler。
- **channel 有界，满了丢弃并计数**：规则引擎不允许阻塞请求热路径。
- **typed 动作**：`Throttle → RuntimeHealth`、`FailAccount → latch + CAS`。不要在动作里直接写库。
- **动作的落库走 `PersistFunc`**（`internal/rulepersist` 提供实现），本包不 import repository。
- **`Kind` 的失败桶语义**：`CountFailureGE` / `RatioFailureGE` 是"非 ok 非 429"（5xx/network/4xx 并入）。

## 验证

```bash
go test ./internal/rule/ -count=1
```
