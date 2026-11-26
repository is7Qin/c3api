# internal/architecture — 依赖图门禁（本仓库最重要的结构性约束）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件讲**门禁机制本身**：
> 它钉什么、怎么加债、为什么你必须先读它再动 import。

## 一句话

`importgraph_test.go` 把「哪个包可以 import 哪个包」写成**显式 allow 矩阵 + 债务账本 + 指纹**，
在 CI 里机械执行。它**不是**碍事的测试，是这个仓库保持分层不腐化的唯一手段。

## 两本硬规则（零豁免）

1. **`pkg/*` 不得 import 任何 `internal/*`**。连 legacy 账本都登记不进去
   （`checkEdges` 的 `isHardRuleEdge` 分支直接判 `kindHardViolation`；`TestImportGraphProduction`
   还有 `require.Falsef(t, isHardRuleEdge(e), "pkg/* -> internal/* 不允许登记 legacy")`）。
   理由：`pkg/` 是可复用层，反向依赖 `internal/` 就失去了可抽出的意义。
2. **未知包默认拒绝**。新包不会因为"没写规则"而放行——它落到 `leafRule`
   （只允许 `internal/domain` / `internal/credential` / `internal/worker` / `pkg/`）。
   新包**必须显式声明**层级。

## `judge()` 的精确语义（改矩阵前必读）

位置：`importgraph_test.go:153-184`。

| 机制 | 行为 | 易踩的坑 |
|---|---|---|
| `ruleFor(pkg)` | 取 **`from` 前缀最长命中**的那条规则（`:146`） | `internal/ent` 与 `internal/ent/schema` 是**两条独立**规则；顺手改前者不影响后者 |
| 未命中 | 才用 `p.fallback`（= `leafRule`，`:232`） | — |
| 命中后 | **fallback 完全不生效**（`:168-173`） | 裸规则（无 `to`）会让该包**所有边被拒**。零内部依赖的包能"碰巧"工作，但那是运气，不是声明 |
| `matchesAnyPrefix` | 遍历 `r.to`（`:217-224`） | **`to: []string{}` 使所有边被拒**（空切片零次迭代）——这正是"叶子包"的正确写法 |
| `matchesPrefix` 两副面孔 | 无尾斜杠：`pkg == prefix \|\| HasPrefix(pkg, prefix+"/")`；**有尾斜杠：纯 `HasPrefix`** | ⚠ 带尾斜杠的前缀**不命中自身**。`"internal/ent/schema/"` 命中零个包（叶包无子包）⇒ `TestImportGraphPrefixHygiene` 红 |
| allow vs legacy | **allow 优先**：`judge` 通过就 `continue`，legacy 根本不被看到（`:1145-1148`） | 见下节 |

## 加债的四步（缺一不可）

```go
// productionLedger / testLedger / externalTestLedger 的 debt 区
{"internal/from", "internal/to", "为什么要欠这条债 + 什么时候还", phaseP4},
```
1. **单独提交**（不要混在功能 PR 里）
2. 登记 `legacy{reason, phase}`。`reason` 必须写清**为什么**、`phase` 只能是 `P2/P3/P4/P6`
   （`validNarrow()`，`:824-830`）
3. **同步调大 `debtMax`**：`len(l.debt) != l.debtMax` ⇒ 精确相等，`:808-814`
4. 在评审里说明理由

⚠ **不要**顺手把 legacy 边同时写进 allow 的 `to`：`checkEdges:1166` / `:1229` 会在"legacy 边变成
allowed"时报 `kindRedundantLegacy` **变红**（"该边已符合目标方向，删除该条并把上限减 1"）。
清债时要**同时**删账本行并把 `debtMax` 减 1。

## 三本账，别搞混

| 账 | 口径（`go list` 的哪个字段） | 谁消费 |
|---|---|---|
| `productionLedger` | `Imports`（生产） | `TestImportGraphProduction` |
| `testLedger` | 同包测试 `TestImports` | `TestImportGraphTestWidening` |
| `externalTestLedger` | 外部测试包 `XTestImports` | `TestImportGraphExternalTestWidening` |

**测试侧的跨层依赖不在债务账里**，走 `testFixtureAllow`（`:513` 附近）——它是**夹具白名单**，
通过 `policy.widened(testFixtureAllow)` 同时加宽 TestImports 与 XTestImports 口径。
条目形如 `"internal/server"`，**上方必须写一段注释**说明"测试要真实装配什么、为什么生产不需要"。
既有先例：`internal/testsupport/`、`internal/ent/`、`internal/db/migrate`、`internal/repository`、
`internal/rulepersist`、`internal/sdkbridge`、`internal/server`、`internal/billing`、`pkg/`。

⚠ **`testOnly = 测试 import − 生产 import`**（`TestImportGraphTestWidening`）。所以一条
"生产与测试共用"的边，若生产侧登记在 `productionLedger`，删掉那行的**瞬间**测试侧就暴露出来 ⇒
必须同时确认测试侧该走 `testFixtureAllow` 还是 `testLedger`。

## 指纹

`allowMatrixFingerprint`（`:658` 附近的 const）是整张策略（`allowRules` + `leafRule`）
规范化后的 sha256。`policyFingerprint`（`:683-700`）**只哈希矩阵，不哈希账本**。

⇒ 判据只有一句：**矩阵动了指纹就变，删债不动矩阵则不变**。
- 只删一条债 → 指纹**不该**变
- 加/改任何 `to`/`deny`/`all` → 指纹**必变**，必须更新常量并在提交信息里说明理由
- **不要预写指纹值**：落地后跑测试拿报错里的 `actual` 再填

另有 `allMax = 5`（`:665`）：`all: true` 的规则条数精确相等、只许改小。
`all: true` 是最危险的放宽方向，新增必须说明为什么不能用 `to:` 白名单。

## 中立叶子包（本轮确立的范式，R2-a / P4-c-2）

当**值类型层**需要一个类型、而另一个实现层也还需要时，把它下沉成**只依赖 stdlib 的叶子包**：

```go
// allowRules 里
{from: "internal/modelmap", to: []string{}},      // 显式空切片
{from: "internal/identityenv", to: []string{}},
```
- 写**显式 `to: []string{}` 而不是裸规则**：语义相同（叶包不依赖任何内部包），但将来有人往
  里加 import 时得到的是这条规则**明确的拒绝**，而不是被 `leafRule` 顺手放行 domain/credential/pkg
- `domain` 侧留**类型别名** `type X = pkg.X`（不是新类型、不是包装），消费方零改动
- 已有先例：`internal/modelmap`、`internal/identityenv`、`pkg/egressapi`、`internal/storeerr`、
  `internal/paymenterr`、`internal/qualitymodel`、`internal/loadport`、`internal/identityfence`、
  `pkg/headervalid`、`internal/pricingresult`、`internal/codexproto`
- 最新一例（request_patch）：头 wire 判据从 `pkg/httpx` 下沉为 `pkg/headervalid`——`internal/domain` 想调两个纯函数，
  不能为此 import 整个 `net/http` 传输装配；矩阵只给 `internal/domain` 的 `to` 加 `pkg/headervalid`（替换原 `pkg/httpx` 边），
  并登记一条显式空 `to` 的 `pkg/headervalid` 规则（`pkg/*` 由硬规则短路，这条是声明式意图）。指纹随矩阵变更，见 archfix 报告。
- 最新一例（P6-1）：`internal/pricingresult` 装价格手动同步/预览的**结果值**与 `FetchError` 错误**类型**，
  让 handler 驱动价格同步而不必认识 pricing 的 fetcher/repo/settings/cron/worker 实现面；消费方用
  `errors.As` 认类型而非认实现方的哨兵变量名。
- P6-2 一例：`internal/codexproto` 装 codex 协议的**纯解析**（`DeriveAccountID`，JWT claims →
  `chatgpt_account_id`）与用量查询失败的**分类类型**（`Error{Kind, Err}`），让 handler 解 account id、
  认用量错误而不必 import `sdkbridge`（该包实测 305 个传递依赖）。
  ⚠ **禁止** `internal/codexproto` import 任何 `internal/*` 或 `pkg/*`——尤其禁止"薄包 sdkbridge"：
  实测 `go list -deps codex-sdk` = 218 个包，一旦薄包，`to: []string{}` 不再成立、中立叶子失效。
  ⚠ JWT 解析在此是**两份实现**（本包 + 第三方 `codexsdk.AccountIDFromToken`）；
  「两份必须语义一致」由 `internal/codexproto/codexproto_test.go` 的 `TestDeriveAccountIDMatchesSDK`
  逐用例对照钉住。这条对照测试是**承重结构**：删掉它，两份实现就可以静默分叉。

### 证明"别名 vs 新类型"必须用无转换赋值

```go
var _ pkg.T = T{}          // ✅ 拦住漏 '='
var _ = pkg.T(T{})         // ❌ 假绿：defined type 与底层类型之间恒可转换
```
**实测**：named type（漏 `=`）**且**在跨包边界补一处转换后，整棵树只有这条赋值断言能拦下——
`go build` 全绿。所以这类断言文件是**承重结构**，不是装饰。

## 验证

```bash
go test -count=1 ./internal/architecture/                                  # 全套（最快 ~3s）
go test -count=1 -run TestImportGraphProduction ./internal/architecture/    # 生产侧
go test -count=1 -run TestImportGraphAllowMatrixFingerprint ./internal/architecture/
go test -count=1 -run TestImportGraphPrefixHygiene ./internal/architecture/  # 前缀拼写
```
