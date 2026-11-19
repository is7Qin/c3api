# internal/credential — 凭据类型词表

## 职责

- 定义 `credential.Type` 与 `IdentityGroup()`：codex-oauth/codex-pat 同组，BPS 独立，
  api_key/custom 各自成组。
- `UsesStaticKey`/`IsCodex`/`IsBPS` 是调用方判断凭据形态的唯一入口。

## 不变量

- 新增类型必须同时定义 `IdentityGroup`、`ValidTemplateExt` 与模板写入/探测分支；
  身份组决定账号查重作用域，不能按名称猜。
- BPS 非静态 key；BPS 模板的 `SupportedFormats` 只允许 Responses 两种格式。

## 验证

```bash
go test ./internal/credential -count=1
```
