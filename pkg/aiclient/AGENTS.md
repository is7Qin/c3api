# pkg/aiclient — 上游 HTTP 出栈（URL 派生 / 凭据注入 / 客户端头中转）

> 根 `AGENTS.md` 覆盖项目级约定与 **ANTI-PATTERNS #2（relayDeny，不可被配置穿透）**。

## 职责

所有协议面（流式 / 非流式 / 图片 / search / ws）**统一走网关自己构造的原始请求**，不再经由
`openai-go` / `anthropic-sdk-go` 客户端——SDK 只作**测试依赖**，用于验证真实客户端能正确消费
本网关产出的字节。

| 文件 | 内容 |
|---|---|
| `aiclient.go` | `Factory` / `NewFactory` / `Config`（模板级客户端**双缓存**：client + URL） |
| `relay.go` | `RelayHeaders` + **唯一一份** `relayDeny` |
| `responses_ws.go` | `AcceptResponsesWS`（WS 升级判定） |

## 不变量

- **透传 = default-allow + 全仓唯一 `relayDeny`**：网关**不做**配置驱动的"客户端头 → 上游头"映射。
  `relayDeny` 不可被配置穿透，扩容须过同级评审。
- **必剔清单**：`Authorization` / `X-Api-Key` / `Cookie` / `Accept-Encoding`（凭据 / 跨租户态 /
  gzip 漏计费）+ 入站足迹族（`X-Forwarded-*` / `Forwarded` / `Via` 等——描述**入站**连接，网关亦
  不追加自身观测）+ `Expect`（入站 100-continue 协商，客户端可注入的延迟放大器）+
  `X-Request-Id`（网关自己写过，递上游 = 泄漏内部关联 id 或让客户端注入未校验值）。
- **写清单须用规范形**：`X-Real-Ip` / `Cf-Connecting-Ip` / `True-Client-Ip` 这类；常见写法能编译
  但**永不命中**。
- **deny-list 不是安全边界**：`X-Client-IP` 等自定义键照旧达上游，**不得宣传为"防 IP 泄漏"**。
- **codex WS 面不喂任何客户端头**：握手头仅来自 SDK。

## 验证

```bash
go test ./pkg/aiclient/ -count=1
go test ./pkg/aiclient/ -run TestRelay -count=1
```
