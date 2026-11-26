# internal/codexproto — codex 协议的纯数据与纯解析（中立叶子包）

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

管理面要做两件事，都不该让 `internal/handler` 认识 `internal/sdkbridge`：

1. **补全 codex account id**——保存单账号 ext 与批量导入时，从 ChatGPT 身份 JWT 的
   payload 里读 `chatgpt_account_id`；
2. **分类用量查询失败**——`/api/admin/accounts/usage` 要把失败分成「凭据失效 /
   上游错误 / 其它」三档，映射成契约里的 `auth_expired` / `upstream_unavailable` /
   不标。

P6-2 之前这两件事都是 handler 直接调 `sdkbridge` 完成的，靠 `productionLedger` 的
`{internal/handler, internal/sdkbridge}` 那条 P6 债豁免。而 `sdkbridge` **实测 305
个传递依赖**（websocket / redis / bps-sdk / codex-sdk / pkg/egress）——为了解一段
JWT、认两类错误，把整套传输栈拖进控制器层的类型依赖。

本包把「协议形状」（一个纯函数 + 一类纯值）拿走，于是双方都不再认识对方的包：

| 关注点 | 生产方 | 消费方 | 共享物 |
|---|---|---|---|
| JWT claims 解析 | —（纯函数，谁都能调） | `handler.deriveAccountID` | `DeriveAccountID` |
| 用量错误分类 | `sdkbridge.classifyUsageErr` | `handler.assembleUpstream` | `Error` / `ErrorKind` |

与 `internal/pricingresult`（P6-1）、`internal/modelmap`、`internal/identityenv` 同范式。

## 职责 / 入口

| 文件 | 内容 |
|---|---|
| `codexproto.go` | `DeriveAccountID(token) (string, bool)` — 从 JWT payload 提取 `chatgpt_account_id` |
| `errors.go` | `ErrorKind`（`ErrKindAuthExpired` / `ErrKindUpstream`）、`Error{Kind, Err}` + `Error()` / `Unwrap()` |

**在线**那一半（PAT whoami）**不在本包**：它是真出站 I/O，实现是 `codexsdk` 的公开面
（`sdkbridge.FetchPATAccountID`）。消费方经 `handler.AccountIDResolver` 端口注入
`sdkbridge.PATWhoami{}`，同样不必 import `sdkbridge`。

## 不变量

- **零内部依赖**：只 import stdlib（`encoding/base64` / `encoding/json` / `strings`）。
  ⚠ **禁止** import 任何 `internal/*` 或 `pkg/*`。矩阵里本包的规则是显式
  `to: []string{}`，加一条内部 import 立刻变红。
- **⚠ 禁止「薄包 sdkbridge 更省事」这条路**。实测 `go list -deps codex-sdk` = 218 个包
  （websocket / zstd / gjson / net/http）。一旦本包薄包 SDK，「中立叶子」失效、
  `to: []string{}` 不再成立，本包存在的意义（切断 handler 的传输栈依赖）随之消失。
- **`DeriveAccountID` 与 `codexsdk.AccountIDFromToken` 必须语义一致**。这是**两份实现**
  （一份在这里、一份在第三方 SDK），一致性不是口头约定：
  `codexproto_test.go` 的 `TestDeriveAccountIDMatchesSDK` 把两者逐用例对照——改一边
  而不改另一边，那条测试立刻红。**这条对照测试是承重结构，不要删、不要改成只测本包**。
- `DeriveAccountID` **不验签、不校验 exp**（过期 access token 的 claims 照样可读——
  补全 account id 不需要它还有效）。任何失败返回 `("", false)`，**不 panic、不返回
  error**：调用方把"派生不出来"当成合法的留空语义（交给 service 层行级必填校验兜底），
  而不是一次需要区分原因的失败。
- **`Error` 用 `errors.As` 判，不要退回哨兵判定**。消费方判的是**类型 + Kind**，不是
  实现方的变量名。实现方（`sdkbridge`）在产出本类型的同时仍用 `%w` 把旧哨兵串进链里
  （`As` 与 `Is` 相互独立），所以「老调用方按哨兵判定」与「新调用方按 Kind 判定」可以
  共存——这正是 sdkbridge 既有测试零改动的根据。
- **`Error` 不包装 `serviceerr.ErrUpstream`**：那个哨兵被 `codex_login` / `payment`
  等多处共享生产，拿它当谓词会把无关上游故障误标成「codex 用量上游错误」。
  ⚠ `sdkbridge.ErrUpstream` 与 `serviceerr.ErrUpstream` 是**同名、不同值、同词义**
  （仓库第二例），别混。
- `ErrorKind` 是**纯值**，不认识展示词（`auth_expired` / `upstream_unavailable`）——
  那是消费方（handler）的映射责任。

## 验证

```bash
go test -count=1 ./internal/codexproto/                                          # 本包全部用例
go test -count=1 -run TestDeriveAccountIDMatchesSDK ./internal/codexproto/       # 与 SDK 的等价性
go list -f '{{join .Imports "\n"}}' ./internal/codexproto                        # 必须只有 stdlib
go test -count=1 ./internal/architecture/                                        # 矩阵（本包 to: []string{}）
go test -count=1 ./internal/handler/ ./internal/sdkbridge/                       # 两侧消费方
```
