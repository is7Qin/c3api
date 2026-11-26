# internal/loadport — 装载端口（控制面 ↔ 存储实现的中立契约）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 为什么单独成包（P5-1 裁决 A）

控制面（`internal/scheduler`）向存储实现（`internal/repository`）索要账号快照时需要一个中立契约。

**不与 `internal/api` 同居**，两条理由：
- **语义不同向**：`api` 是"数据面 ↔ 控制面"的契约（`Selection` / `SelectionHandle` /
  `AttemptPlan`）；本包是"控制面 ↔ 存储实现"的契约。方向不同，不该共用一个包名去解释。
- **硬约束**：`api` 自己 import `internal/sdkbridge`，而 `sdkbridge` import `internal/repository`
  ⇒ `repository → api` 是 Go **导入环**（编译期直接失败）。装载端口的实现方必须能 import 契约包，
  不能反过来。

## 导出面

| 符号 | 说明 |
|---|---|
| `BatchedGroupLoader` | 批次装载接口 |
| `GroupAccountBatch` | 批次会话（控制面只持会话，作用域在实现包内） |
| `ExtCredentialMismatch` | 凭据类型与 ext 行不匹配的判定值 |

## 不变量

- **控制面只持"会话"**，不知道作用域细节——这使 `scheduler` 不再需要 `internal/repository`
  （账本里那条边因此消失）。
- 本包只放**契约**，不放实现。实测依赖面：`context` + `internal/credential` + `internal/domain`（纯值类型层）。

## 验证

```bash
go test ./internal/loadport/ -count=1
```
