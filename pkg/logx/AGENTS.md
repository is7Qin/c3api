# pkg/logx — 日志（zap 的唯一允许入口）

> 根 `AGENTS.md` 覆盖项目级约定与 **ANTI-PATTERNS #1**。

## 职责

`logx.go` 的 `Logger` / `New` + 字段构造器（`String` / `Int` / `Int64` / `Bool` / `Duration` /
`Any` / `Error` / `Field`）：zap 的**薄包装**。

## 不变量（硬规则）

- **业务代码只允许经本包取日志，禁止直接 import zap**。这是根 `AGENTS.md` ANTI-PATTERNS 第 1 条。
- 本包是 zap 的唯一持有者（`go.uber.org/zap` / `zapcore` 只出现在这里）。
- 字段构造走本包的辅助函数，不要在调用点裸造 `zap.Field`。

## 验证

```bash
go test ./pkg/logx/ -count=1
```
