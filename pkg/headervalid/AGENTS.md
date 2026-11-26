# pkg/headervalid — 出站 HTTP 头 wire 合法性判据（中立叶子包）

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

`request_patch`（custom 模板出站请求修饰器）的保存期校验需要判断「这个头名/头值能不能上 wire」：
`internal/domain` 的 `ParseRequestPatch` 要校验模板配置，`pkg/aiclient` 的 `RelayHeaders` 要剔掉入站脏头值。
两处必须是**同一份判据**（两份会漂移：入站放行的值保存期拒绝，或反之），但 `internal/domain` 是值类型层——
它只想调两个纯函数，却不能被拖进 `pkg/httpx` 的 `net/http` 传输装配（Go 的 import 粒度是包）。
因此判据下沉到本包：stdlib-only 的中立叶子包，作为全仓唯一单源（判据原在 `pkg/httpx/header.go`，archfix 时搬到这里）。

## 硬边界

只 import stdlib（`go list` 实测 `imports=[]`）。**不得** import 任何 `internal/*`（架构门禁的 `pkg/*` 硬规则，零豁免），
**也不得** import `pkg/httpx`——那等于把判据又绑回传输装配层，搬家的意义归零。allow 矩阵有显式 `to: []string{}` 声明
（`pkg/*` 边由硬规则短路，这条是声明式意图，见 archfix 报告）。

## 导出面

- `ValidHeaderName(s string) bool`：RFC 9110 §5.1 token（`ALPHA` / `DIGIT` / `!#$%&'*+-.^_`|~`）；空串非法；
  **不做规范化**，调用方负责 `http.CanonicalHeaderKey`（保存期与运行期比较都按规范形）。
- `ValidHeaderValue(s string) bool`：对可出站 field-value 的卫生判据——允许可打印 ASCII、TAB 与 UTF-8 高位字节；
  拒绝 0x7f（DEL）与 <0x20 的非 TAB 控制字节（CR/LF/NUL 等）。空串合法（空值在 `request_patch` 里另表 Del 语义）。

## 不变量

- **只陈述 wire 合法性，不携带任何策略**：`Accept-Encoding` 的保存期拒绝（响应编码不变量）、`{api_key}` 占位符、
  头名归一都在消费者（`internal/domain` / `pkg/aiclient`）里，不在本包——否则就成了「两份判据」里的另一份。
- **等价性冻结**：`ValidHeaderValue` 必须与 `pkg/aiclient` 旧私有 `valueClean` 逐字节等价（TAB 放行、其余 <0x20 与 0x7f 拒绝）。
  旧实现只留在 `headervalid_test.go` 的 `legacyValueClean`（测试内历史副本），表驱动等价测试覆盖 0x00–0xff 单字节与全部双字节组合；
  改判据会红。
- 判据是纯函数：无状态、零分配、可在任意 goroutine 并发调用。

## 验证

```bash
go test ./pkg/headervalid/ -count=1
go list -f '{{.ImportPath}} imports={{.Imports}}' ./pkg/headervalid   # 只许 stdlib
go test ./internal/architecture/ -count=1                             # pkg/* 不得 import internal/*
```