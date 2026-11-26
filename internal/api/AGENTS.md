# internal/api — 数据面 ↔ 控制面的中立契约

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲这条契约的边界。

## 为什么有这个包

`internal/proxy`（数据面，/v1/* 热路径）与 `internal/scheduler`（控制面，选号快照）需要交换
"这次派发用哪个账号/模板"的事实，但**数据面不能 import 控制面**（会把整棵调度器拉进热路径的
依赖闭包，且反向成环）。

所以这里只放**四样东西**，全是值类型与哨兵：

| 符号 | 形状 | 说明 |
|---|---|---|
| `Selection` | 结构体（实测 20 个只读字段） | 一次派发的**具体结构**。数据面直读字段、零成本。**它不是控制面对象**——私有租约/会话状态在 `Selection.Handle` 背后 |
| `SelectionHandle` | 接口，**恰好 4 个方法**（`Release` / `Thread` / `StepIdentity` / `Credential`） | 控制面私有句柄。数据面只经它触碰生命周期，别的一概不暴露 |
| `AttemptPlan` | 接口，**恰好 2 个方法**（`ContinuationIncompatible` / `AbandonLastAttempt`） | 计划的窄消费面。`ReserveAttempt` 属控制面编排，**不在**计划接口上 |
| 运行期选号哨兵 | `errors.go` | 组不存在 / 格式不可路由 / 计划未就绪 / 账号不可用 / 计划尝试耗尽 |

`HandleOrDetached` 是数据面**唯一**的句柄取值口：`nil` 或零值 `Selection` ⇒ 分离句柄。

## 不变量

- **依赖方向单向**：本包**不 import** `internal/scheduler`。门禁 F2 机械强制；控制面把 `Selection`
  别名为自己的类型并向数据面提供 handle 实现，数据面只认本包。
- **哨兵的 owner 只能在这一层**：生产方在控制面、判定方在数据面，跨包判等用的是 **error 值身份**。
  所以 `scheduler` 侧只留 `var ErrX = api.ErrX` **别名**（不是新 `errors.New`——那会让跨包 `errors.Is` 恒假）。
- **文案里的 `scheduler:` 前缀是历史日志口径**，不随归属迁移改变。不要"顺手改成 `api:`"。
- **导出面纪律**：不导出可写状态。`Selection` 的字段是只读事实；生命周期只能经 `SelectionHandle`。

## 与 `internal/loadport` 的分工（别混）

| 包 | 契约方向 |
|---|---|
| `internal/api` | 数据面 ↔ 控制面（`Selection` / `AttemptPlan`） |
| `internal/loadport` | 控制面 ↔ 存储实现（装载端口 `BatchedGroupLoader` / `GroupAccountBatch`） |

两者语义不同向，**不该合并**。硬约束：`api` 自己 import `internal/sdkbridge`，而 `sdkbridge` import
`internal/repository` ⇒ `repository → api` 是 Go 导入环（编译期直接失败）。所以装载端口的实现方
必须能 import 契约包，不能反过来。

## 验证

```bash
go test ./internal/api/ -count=1
go test ./internal/api/ -run "TestSelectionShapeIsConcreteFacts|TestSelectionHandleIsExactlyFourMethods" -count=1   # 契约形状钉死
```
