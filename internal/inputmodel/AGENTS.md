# internal/inputmodel — handler 侧的查询 / 补丁 DTO

> 根 `AGENTS.md` 覆盖项目级约定；分层见 `internal/architecture/AGENTS.md`。

## 为什么有这个包（R1-c）

`handler` 需要描述"用户想查/想改什么"，但它不该为此 import `internal/repository`。把这些 DTO
下沉成只依赖 `domain` 与 `time` 的叶子包后，`handler` 与 `repository` 各取所需。

## 导出面

| 文件 | 类型 |
|---|---|
| `queries.go` | `ListQuery`(13) / `UsageQuery`(11) / `OrderQuery`(7) / `ErrLogQuery`(12) |
| `patches.go` | `GroupPatch`(2) / `TemplatePatch`(6) / `AccountPatch`(11) / `UserPatch`(7) |
| `pricing.go` | `PriceEntryManual`(10) |

括号内是字段数（改动时会有测试钉住）。

## 不变量

- **指针即"写入意图"**：`nil` = 未提供（跳过），非 `nil` = 精确落值（**含零值**）。`0` 与"未提供"
  必须可区分。
- **字段集合是契约**：`handler` 与 `repository` 用同一份；不要在其中一方私有复制。

## 验证

```bash
go test ./internal/inputmodel/ -count=1
```
