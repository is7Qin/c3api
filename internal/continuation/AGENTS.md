# internal/continuation — Responses 续接绑定

> 根 `AGENTS.md` 覆盖项目级约定；Redis 承载的可丢易失协调状态见 `pkg/redisx` 与 `internal/AGENTS.md`。

## 职责

`store.go` 的 `Store` / `New` / `Binding` / `BindRequest` / `BindResult`：把
（previous_response_id / 续接键）绑定到具体的（账号, 指纹, 代际）。Redis 键族 `codex:conv:*` /
`codex:turn:*` 属本包语义面。

## 不变量

- **绑定以 (指纹, K) 判定**：`LifecycleRevision`（C）变化会击穿在途 binding；纯配置写入只动 C 的
  语义边界见 `domain.Account` 字段注释。
- **Redis 只承载可丢的易失协调状态**，永不作为 SoR / 缓存层 / 失效总线。
- 与 `internal/latch`（单实例内存锁存）职责分离：latch 是单实例内存，**与 Redis 无关**。

## 验证

```bash
go test ./internal/continuation/ -count=1
```
