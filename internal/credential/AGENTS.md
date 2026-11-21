# internal/credential — 凭据类型词表

## 职责

- 定义 `credential.Type` 与 `IdentityGroup()`：codex-oauth/codex-pat 同组，BPS 独立，
  api_key/custom 各自成组。
- **形态由 `capsTable`（`caps.go`）单点定义**；`UsesStaticKey`/`IsCodex`/`IsBPS` 等方法是
  它的只读投影。**分发**一律走 `ChannelOf`——调用方按通道分支，不再问类型谓词
  （`internal/proxy` 侧由 `credential_channel_gate_test.go` 的 AST 门禁禁止）。
- `UsesStaticKey`/`IsCodex`/`IsBPS` 仍是调用方判断凭据形态的入口（本轮不改 84 处调用点）；
  但**新**代码要分发时用 `ChannelOf`，别加新的 `Is*` 谓词。
- 本包**不再承载凭据分发**：`Registry`/`Provider`/`New`/`For` 已删。静态通道的取值由
  `internal/proxy` 的 `credentialFor` 按 `UsesStaticKey()` 直读 `Selection.UpstreamKey`
  （`credential_for_test.go` 锁住返回值与两条错误文案）；别为「以后可能有 HTTP 面多头」
  复活恒等层——真要支持时按 `ChannelOf` 加分支。
- `ErrUnsupported` 仍常驻本包作为**跨包 sentinel**，不得随 Registry 一起删：
  `internal/sdkbridge/bps.go` 在包它，`cmd/server/wiring_billing.go` 在 `errors.Is` 它
  （BPS 探测的凭据不可用 → 400）。

## 不变量

- 新增类型的动作 = `capsTable` 加一行 + `IdentityGroup` 补 case + 同步 `caps_test.go` 的
  两张手抄矩阵（`TestTypeBehaviorFrozen` / `TestChannelFrozen`）。
  漏改**表行或任一 golden** → 红；但只加 `Type` 常量、不加表行 → 编译通过、全门禁绿，
  此时 `Valid()` 恒 false，该类型被上游 fail-closed 显式拒绝，不会被静默选中。
  caps 只剩 `ch` + `validTplExt` 两列后**不存在跨列不变量**（两者无函数关系）：
  **golden 是唯一的跨类型锁**；表外语义（7 个布尔谓词恒 false、`IdentityGroup` 自成
  一组）由 `TestPredicatesOnUnknownType` / `TestIdentityGroupOnUnknownType` 单独咬住。
- 身份组决定账号查重作用域，不能按名称猜。
- 包内不许新增按类型判定的 helper（`TestCredentialPkgOwnsNoTypePredicateHelper` 反向门禁）。
- BPS 非静态 key；BPS 模板的 `SupportedFormats` 只允许 Responses 两种格式。

## 验证

```bash
go test ./internal/credential -count=1
```
