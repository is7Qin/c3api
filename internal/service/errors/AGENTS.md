# internal/service/errors — service 层错误哨兵（`serviceerr`）

> 根 `AGENTS.md` 覆盖项目级约定；与 `storeerr` / `paymenterr` 的分工见各自 `AGENTS.md`。

## 为什么是独立叶子包

**叶子包（零内部依赖）**：不 import `internal/service` 或任何上层包，否则形成
`service → 本包 → service` 的 **import 环**。

`service` 包以**别名 re-export**（`var ErrNotFound = serviceerr.ErrNotFound` 等）保持既有引用
（`errors.Is(err, service.ErrXxx)`）**同一哨兵实例**语义——80+ 调用点零改动。

## 不变量

- **8 个哨兵**（package 名是 `serviceerr`，目录名是 `errors`）。
- **必须是同一 error 值**：`service` 侧只留别名，**不要** `errors.New` 新实例。
- `StatsWindowError` 是带机读字段的类型化错误（`httpface.WriteServiceErr` 用 `errors.As` 取它，
  **禁止字符串嗅探**）。
- **HTTP 状态映射表在 `internal/httpface`**，不在本包。

## 验证

```bash
go test ./internal/service/errors/ -count=1
```
