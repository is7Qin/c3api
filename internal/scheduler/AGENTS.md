# internal/scheduler — 选号快照、预留与 Codex 槽位身份

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲调度器
> 的入口、快照纪律与 Codex 相关的预留语义。

## 职责 / 入口

- 内存优先的账号调度：规则事件投递（`internal/rule` 的 typed Throttle/FailAccount）、
  预编译路由计划执行、账号并发槽、静态快照（`StaticView`）与出口解析。
- 选号入口：`NewAttemptPlan` / `NewAttemptPlanWithCacheAffinity`（`attempt_plan_reservation.go`）
  → `ReserveAttempt(plan)` 返回 `*Selection` + `Attempt`；proxy 失败转移靠计划推进。
- `Selection`（`scheduler.go`）：选中账号的只读视图 + 租约。Codex 相关：
  - 槽位路径：`IdentityFor(owner, replay)` 取槽位会话身份；`StepIdentity()` 仅在成功取到
    usage 且上游连接已归还后调用一次；
  - 会话路径（本轮新增）：`Conversation`（= `AttemptPlanIdentity.Conversation`）、
    `CacheDomain`（生效缓存域，私有域 = `private:<账号ID>`）、`InstallationID`。
- 槽位池：`identity_pool.go`（`claimCodexSession`、上界 `maxIdentityPoolSlots` 1024）；
  跨实例账号并发视图：`concsync.go`。

## 关键不变量

- 已发布快照只能 copy-modify-Store；disabled 账号不得被在途事件复活。
- 选号路径 100% 本地判定、**零 Redis**（Redis 只被 concsync worker 每 tick 一条
  pipeline 读写）；零 DB、零每请求锁。
- 预留零新增分配：`TestClaimAndIdentityZeroAlloc` 恒为 0；`NewAttemptPlan + ReserveAttempt
  + Release` 在 BASE 上实测 5 次分配，带不带 `Conversation` 都不得超过（C3-15）。
- `AttemptPlanIdentity.Conversation=true` 时 `reserveOnView` **不认领** Codex 槽位：槽位池
  无占用、无推进，`IdentityFor` 返回零值。会话路径的身份由 SDK 会话簿合成，所以
  installation 只能从 `Selection.InstallationID` 取，不得复用 `IdentityFor`（零值语义是
  “匿名”，复用会静默变成不带身份）。
- `cacheDomain` / `installationID` 在快照构建期（`newSnapshotStatic`）一次定型，**仅 Codex
  凭据**填，非 Codex 为空；预留只做字段拷贝（私有域拼接放进预留会每次分配）。源字段已在
  静态键里，换值即换叶子。
- `MaxConcurrency=0`（无池）时预留不 panic，按现状零身份。

## 边界

- 会话簿（Redis 共享的对话身份）在 SDK 与 proxy，不在本包；本包只给出“是否走会话路径”
  与账号级事实。`Conversation` 的入口置位（proxy caller）留给接入第二轮。
- 出口 client 来自 `pkg/egress`（快照解析成 `*egress.Client` 指针）；连接回收在快照
  reconcile。

## 验证

```bash
go test ./internal/scheduler/ -count=1
go test ./internal/scheduler/ -run 'TestConversationSkipsSlotClaim|TestReserveConversationNoNewAllocs|TestClaimAndIdentityZeroAlloc' -count=1
```

## 最近变更

- 2026-11-13 `9524d5a`、`e24aa01`（codex-conversation-integration §5.2，C3-3/C3-15）：
  `AttemptPlanIdentity.Conversation`、`Selection.Conversation/CacheDomain/InstallationID`、
  快照期 `cacheDomain/installationID`；会话路径跳过槽位认领。接入第二轮还会改：
  proxy 入口置位、book 为 nil 时回到槽位。
