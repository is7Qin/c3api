# internal/statusenum — 实体状态枚举的唯一权威（active / disabled）

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

这些值有两类消费者，而它们**不能互相依赖**：

- `internal/ent/schema` —— Ent 枚举的声明处，是 **codegen 的输入**。
- `internal/domain` —— 业务类型层。

原先值放在 `domain`，schema 只能 import `domain` 取值。代价实测：`ent/schema` 的
传递依赖从 113（只用 entgo）涨到 **225**，因为 `domain` 自身拖进 `crypto/fips140/*`、
`net/http`、`tidwall/sjson` 等 211 个包。一个只声明几个字符串常量的 codegen 输入包，
不该为此背整套运行时。

把值放到中立叶子包后，`domain` 做**类型别名**，`ent/schema` 直接引用本包：
单一事实源不变，依赖方向变干净。`ent/schema` 的依赖闭包降到 **145**。

## 职责 / 入口

| 类型 | 值 |
|---|---|
| `UserStatus` | `active` / `disabled` |
| `KeyStatus` | `active` / `disabled` |
| `ManagementKeyStatus` | `active` / `disabled` |
| `RedemptionStatus` | `active` / `disabled` |

各有一个 `Valid()`，随类型住在本包（别名不能在新包外定义方法）。

## 不变量

- **零内部依赖**：只 import stdlib（实测当前**一个 import 都没有**）。加任何
  `internal/*` 或 `pkg/*` 之前先问：这两个字符串还需要知道别的什么吗？
- **值一经发布不可随意改**：改了就要同步 `internal/db/migrate/**` 的 DDL 默认值，
  否则 `internal/ent/schema/status_literal_gate_test.go` 的门禁 (b) 会红
  （它从本包现算期望值，与迁移 SQL 比对）。
- **schema 里不许写字面量**：`"active"` / `"disabled"` 直接出现会被门禁 (a) 抓；
  必须写 `string(statusenum.XxxActive)`。
- **不要在这里加业务方法**：它只回答"有哪些合法值"。判定业务含义归 `domain`。

## 验证

```bash
go test ./internal/statusenum/ -count=1        # 目前无独立测试，值由下面两道门禁覆盖
go test ./internal/ent/schema/ -count=1        # U3 双门禁 (a) 字面量 / (b) DDL 漂移
go test ./internal/architecture/ -count=1
go list -f '{{join .Imports "\n"}}' ./internal/statusenum | rg -x 'github.com/is7qin/c3api/.*' || echo "零内部依赖 OK"
```
