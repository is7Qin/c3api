# internal/storeerr — 存储实现层的错误哨兵（单一真相）

> 根 `AGENTS.md` 覆盖项目级约定。为什么"错误按轴单独成叶子包"见 `internal/architecture/AGENTS.md`。

## 为什么有这个包（R1-a）

哨兵是**调用方用来分类的控制流**，不是实现细节。`internal/rule` 与 `internal/sdkbridge` 需要
`errors.Is` 判定，却不该为此 import 整个存储实现层（会把 `ent`、`pgx`、事务门面全部拉进依赖闭包）。
方向固定：**实现 → 契约**。

## 归属判据（最重要，别把东西塞回来）

> **只有"删掉业务语境后依然成立"的存储协议分类才进本包。**

| 哨兵 | 在这里？ | 理由 |
|---|---|---|
| `ErrNotFound` | ✅ | 任何存储都有 |
| `ErrConflict` | ✅ | 任何存储都有（唯一冲突） |
| `ErrStaleRevision` / `ErrStaleIdentityRevision` | ✅ | 任何存储都有（并发代际陈旧） |
| `ErrInvalidArgument` 类 | ✅ | 删掉业务语境仍成立 |

**不在这里的**（带业务策略含义，即使只有一个消费者也留域内包）：
- `qualitymodel.ErrRoutingSnapshotBeyondRetention` —— 编码**保留期策略**，且无 `repository:` 前缀
- `paymenterr.ErrMerchantRefImmutable` —— 被**有意映射成 400**（不是 409）⇒ 业务配置错误

判据是"**删掉业务语境后它还成立吗**"，不是"它长得像不像存储错误"。

## 不变量

- **零内部依赖**（实测只 import `errors` + `fmt`）⇒ allow 规则可以是裸的，但**仍建议显式**
  （见 `internal/architecture/AGENTS.md` 的裸规则陷阱）。
- 新增哨兵前先问：它在 `storeerr` 还是域内包？

## 验证

```bash
go test ./internal/storeerr/ -count=1
```
