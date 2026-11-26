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
| `responses_ws.go` | `AcceptResponsesWS`（WS 升级判定）+ `ResponsesWSDial`（上游拨号，含 request_patch 的握手装饰缝） |

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
- **出站装饰缝 `decorate` 是可信配置面，不是入站透传**：6 个 HTTP 出站方法（`RawPost` / `ChatCompletionRaw` / `ResponseRaw` / `AnthMessageRaw` / `SearchRaw` / `ImagesRaw`）末参是 `func(*http.Request)`，`ResponsesWSDial` 末参是 `func(*websocket.DialOptions)`；`rawPostCT` 的契约顺序是 `RelayHeaders(in) → Content-Type → 鉴权头 → 缺省 Accept / Anthropic-Version → decorate → 强制 Del Accept-Encoding`。装饰器只由 proxy 构造（`headerDecorator` / `wsHeaderDecorator`），**本包不收 key**——`{api_key}` 由调用方闭包用当次 attempt 原始 cred 展开；nil = 无配置快路径（0 分配）。出站 `Accept-Encoding` 的强制剔除是响应编码不变量（上游必须回明文，否则 relay/usage/转换静默拿压缩字节），保存期已在 `domain.ParseRequestPatch` 400，这里是第二道。
- 与 `relayDeny` 的分工：`relayDeny` 只剔**入站客户端头**（不可被配置穿透）；`decorate` 执行在它之后、是模板级可信配置（见根 ANTI-PATTERNS #2/#12）。

## 验证

```bash
go test ./pkg/aiclient/ -count=1
go test ./pkg/aiclient/ -run TestRelay -count=1
```
