# internal/identitypool — codex 会话池（账号级 K 会话）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

一个会话 = 一条上游线程的完整生命周期（installation / thread / window 计数 / 连接身份）。
身份不再是客户端的构造期配置，而是**请求数据**：池把会话摊开成 `Identity` 交给一次调用。

## 并发模型（关键，别改成 channel/mutex）

- 认领 = **旋转游标 + 逐槽 CAS**；归还 = `Store(false)`。**无 channel、无 mutex、请求路径零每请求锁**。
- 会话状态字段是**普通字段**：`busy` 独占保证租借期内只有一个读者/写者，所以不再套 atomic。
- ⇒ **租借之外可读的字段必须不可变**：轮换策略属于池的构造参数，`policy` 只是建会话时抄下的
  只读副本。`Resize` **不写**已发布的会话（容量变化只迁移会话指针；策略变化开新会话并回收旧线程）。
  兜底临时身份也只读池字段，不读 slot。

## 导出面纪律

**不导出任何可写状态。** `Session` 的所有字段不导出，对外只有生命周期方法（`Step` / `Release`）
与只读派生值（`Key` / `Thread` / `Policy` / `Busy`）。**没有** `SetBusy` / `Slots()` 之类写入口
——独占协议只能经 `Claim` / `Release`，调用方无法在 API 层绕过。

## 与调度器的三处缝

`Spec`（构造参数）、`Pool.Claim`（领取）、`Reclaimer`（锁内只收集退役）。
池**不知道**账号配置、出口 client、资源批次——那些属调度器的 `poolReclaim`；池只知道
"某账号某线程该退役了"。

## 验证

```bash
go test ./internal/identitypool/ -count=1
```
