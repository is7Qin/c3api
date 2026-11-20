# internal/credential — 凭据类型词表

## 职责

- 定义 `credential.Type` 与 `IdentityGroup()`：codex-oauth/codex-pat 同组，BPS 独立，
  api_key/custom 各自成组。
- **形态由 `capsTable`（`caps.go`）单点定义**；`UsesStaticKey`/`IsCodex`/`IsBPS` 等方法是
  它的只读投影。**分发**一律走 `ChannelOf`——调用方按通道分支，不再问类型谓词
  （`internal/proxy` 侧由 `credential_channel_gate_test.go` 的 AST 门禁禁止）。
- `UsesStaticKey`/`IsCodex`/`IsBPS` 仍是调用方判断凭据形态的入口（本轮不改 84 处调用点）；
  但**新**代码要分发时用 `ChannelOf`，别加新的 `Is*` 谓词。

## 不变量

- 新增类型的动作 = `capsTable` 加一行 + `IdentityGroup` 补 case + 同步 `caps_test.go` 的
  两张手抄矩阵（`TestTypeBehaviorFrozen` / `TestChannelFrozen`）；漏改任一处即红。
  `TestCapsColumnInvariants` 是不依赖 golden 的第二把锁：表与 golden 一起改错时仍然咬人。
- 身份组决定账号查重作用域，不能按名称猜。
- 包内不许新增按类型判定的 helper（`TestCredentialPkgOwnsNoTypePredicateHelper` 反向门禁）。
- BPS 非静态 key；BPS 模板的 `SupportedFormats` 只允许 Responses 两种格式。

## 验证

```bash
go test ./internal/credential -count=1
```
