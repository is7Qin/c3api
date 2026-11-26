# internal/settingssnap — settings 全量内存快照

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

`settingssnap.go` 的 `Snapshot` / `New` / `SettingsLoader`：settings 全量内存快照的**唯一事实源**。
`Service` 与 `MailWorker` 同源共享单个 `*Snapshot`（单指针，无双快照分叉）。

## 不变量

- **叶子包**：仅依赖 `internal/domain` + `pkg/logx` + 标准库。**不 import `internal/repository`**
  （防环——`SettingsLoader` 窄接口由 repos/fakeStore 结构性满足）。
- **单指针共享**：不要造第二个快照实例，否则两处会漂移。

## 验证

```bash
go test ./internal/settingssnap/ -count=1
```
