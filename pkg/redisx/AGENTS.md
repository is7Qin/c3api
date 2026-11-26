# pkg/redisx — 全仓唯一 Redis 客户端构造点

> 根 `AGENTS.md` 覆盖项目级约定。

## 职责

`redisx.go` 的 `Open` / `Close` / `Options`：`cmd/server/main.go` 调 `Open`，产物
`*redis.Client` 以**构造注入**分发。

## 不变量

- **任何包不得自建第二个客户端**。review grep 点：除本包与 `main` 外禁止出现 `redis.NewClient`。
- **不做命令级包装层**：`miniredis` 已解决可测性，接口是投机抽象；消费方直接用具体方法。
- **Redis 只承载可丢的易失协调状态**，**永不**作为 SoR / 缓存层 / 失效总线。

## Redis 键族一览（跨包）

| 键 | 属主 |
|---|---|
| `c3api:cluster:members` | `internal/discovery`（实例心跳 ZSET） |
| `c3api:emailcode:<purpose>:<email>` | `internal/verification`（验证码 HASH） |
| `codex:conv:*` / `codex:turn:*` | `internal/continuation`（Responses 续接绑定 / Codex 会话簿） |
| BPS token record | `bps-sdk/redisstore`（**c3api 不写**） |

## 验证

```bash
go test ./pkg/redisx/ -count=1
```
