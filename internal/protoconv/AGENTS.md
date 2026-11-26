# internal/protoconv — 协议转换（只补差语义）

> 根 `AGENTS.md` 覆盖项目级约定与六方向边界细节；`internal/AGENTS.md` 是分层地图（"协议转换"
> 一行指向这里）。本文件只讲**改这个包时必须知道的结构与陷阱**。

## 职责 / 入口

| 入口 | 位置 | 说明 |
|---|---|---|
| `ConvertRequest(dir, body)` | `protoconv.go` | 客户端协议请求体 → 模板协议请求体 |
| `ConvertResponse(dir, body)` | `protoconv.go` | 模板协议响应 → 客户端响应（**非流式** JSON） |
| `NewStreamMapper(dir, ...)` / `StreamMapper.Map` / `.Finish` | `protoconv.go` | **流式** SSE 事件映射 |
| `Capability(dir)` / `dirCaps` | `protoconv.go` | 方向能力三元组 `{Request, Response, Stream}`，未登记 → 零值 → **fail-closed** |
| `noteReqDrop` / `noteRespDrop` | `dropsink.go` | 丢弃计数（见下"丢弃必须计数"） |
| `SetDropSink` / `ResetDropCounts` / `DropCounts` | `dropsink.go` | 可观测面与测试复位 |

`ErrUnsupportedDirection` 是"方向未接线"的**唯一 sentinel**：调用方一律 `errors.Is`，
**禁止字符串匹配**（service 配置校验、proxy 流式 fail-closed 守卫都靠它区分"未支持方向"与"输入非法"）。

## 不变量

- **零 SDK 耦合**：只用 `encoding/json`，不 import OpenAI/Anthropic SDK。缺名帧的事件名推断与
  `pkg/sserelay.InferEventName` 共用（热路径零分配锚定 + 回退全量解码）。
- **off 方向不经过本包**：热路径分支在 `internal/proxy` 判定（`groups.protocol_convert` 快照值）。
- **WS 帧流不做转换**：`resp-ws` 是 1:1 透传范围。
- **能力表 fail-closed**：新方向按里程碑逐项打开（请求面 → 响应面 → 流式）。未 GA 时配置层
  400、流式入口在**写 SSE 头之前** 400——宁可不支持，也不给客户端 `200 + 空流`。
- **请求侧全部字节级（gjson），响应侧只有 chat→resp 字节级**：其余响应方向是 `decodeObj` 的
  map-based。**响应侧不对称是有意的，别"统一"**（见根 `AGENTS.md` CONVENTIONS）。
- **丢弃必须计数（三元组：方向 + 面 + 字段）**。面不能省——`domain.ProtocolConvert` 表达的是
  **协议对**（`mess_to_chat` 既是"客户端 mess 请求 → chat 请求"，也是"chat 响应 → mess 响应"），
  少了面这一维就分不清"客户端传的文档被丢"还是"上游返回的文档被丢"。
  - 面的归属由**调用点所在文件职责**决定：请求转换文件（`mess_chat_req.go` / `resp_chat_req.go`）
    调 `noteReqDrop`；响应转换文件（`chat_stream.go` / `chat_upstream_resp.go`）调 `noteRespDrop`。
  - 编译器只能强制"必须二选一"，**拦不住选错面** ⇒ 真正的守卫是文件归属 + 落点测试
    （`dropsink_test.go` 的 `|req|` / `usageclamp_test.go` 的 `|resp|` 断言）。
  - 计数键用**可比较的事件值**而不是拼接字符串（逐块热路径，拼串每次都要分配）。
- **有状态字段判 `polReject` 整请求 400**（如 `previous_response_id`），不许静默丢。
  内容模型判 `polDrop` 的块（document 非纯文本源、search_result、redacted_thinking、file 图片、
  input_file、加密思考）走 `note*Drop`。
- **包级状态是串行的**：`ResetDropCounts` / `SetDropSink` 操作包级变量，测试**必须串行独占**，
  **禁 `t.Parallel()`**（本仓库全仓 `t.Parallel()` = 0）。

## 方向命名双轨（别混）

- `ProtocolConvertXToY` = **客户端说 X、上游说 Y**
- 转换函数按**数据流**命名（`respToChatResponse` = 把 resp 响应转成 chat 响应）

## 文件导航

| 文件 | 内容 |
|---|---|
| `req_fast.go` | 请求侧字节级：`chatToMessRequest` / `messToRespRequest` / `respToMessRequest` |
| `chat_resp.go` | `chatToRespRequest`（请求侧字节级） |
| `mess_chat_req.go` / `resp_chat_req.go` | chat 源两方向的请求侧 |
| `chat_upstream_resp.go` | `respToChatResponse`（**唯一**字节级响应转换） |
| `chat_stream.go` | chat 源的流式帧自解析 |
| `contentmodel.go` | 内容模型判据（`polDrop` / `polReject`） |
| `dropsink.go` / `toolschema.go` | 丢弃与 schema 观测口径 |
| `reasoning.go` / `assemble.go` / `jsraw.go` | 思考块、响应重组、JSON 原始读写 |

## 验证

```bash
go test ./internal/protoconv/ -count=1                       # 38 个测试文件，全量
go test ./internal/protoconv/ -run "TestDropsAreVisible|TestNoteDrop" -count=1   # 丢弃计数落点
go test ./internal/protoconv/ -run "TestCapability|TestUnsupportedDirection" -count=1  # 方向能力表
```
