# internal/rulepersist — 规则判定落库的实现

> 根 `AGENTS.md` 覆盖项目级约定。为什么单独成包见 `internal/architecture/AGENTS.md`。

## 职责

`rule_persist.go` 的 `NewRulePersistFunc`：为 `internal/rule` 的 `PersistFunc` 提供真实实现
（写规则判定结果）。

## 不变量

- **它是 `rule` 与 `repository` 之间的桥**：`rule` 只持 `PersistFunc` 函数类型，不 import repository。
  本包把两者接起来，所以它**允许** import `repository`（allow 矩阵里显式登记）。
- **判定身份用 `identityfence` 的哨兵与指纹**，不要自造第二套（那会让跨包 `errors.Is` 恒假）。
- **锁存用 `internal/latch`**。

## 验证

```bash
go test ./internal/rulepersist/ -count=1
```
