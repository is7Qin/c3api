# internal/identityenv — codex_identity 线缆契约（中立叶子包）

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

`ent` 要表达 `account_ext.codex_identity` 这个 JSON 列（`AccountExt.CodexIdentity`），而 `ent` 不能
反向依赖 `internal/domain`。与 `internal/modelmap` 同型（R2-a）。

## 硬边界

只 import stdlib（`bytes` / `crypto/sha256` / `encoding/json` / `fmt` / `io` / `sort` / `strconv` /
`strings`）。**不得** import 任何 `internal/*` 或 `pkg/*`。allow 规则 `to: []string{}`。

## 导出面

`IdentityEnvelope`（`map[string]json.RawMessage`）/ `IdentityPatch` / `ParseIdentityEnvelope` /
`MergeIdentityEnvelope` / `CanonicalJSON` / `CanonicalIdentityJSON` / `CanonicalJSONHash` /
`IdentityJSONEqual` + envelope 方法（`Clone` / `InstallationID` / `BPSProfile` /
`MergeCodexInstallation` / `ReplaceBPSProfile` / `MarshalJSON` / `UnmarshalJSON`）。

## 不变量

- **未知键必须保留**（`map[string]json.RawMessage`）：旧写入者不能抹掉新版本的 BPS profile 数据。
- **merge-on-write**：`IdentityPatch` 的 nil 字段 = "不动"；`SetBPSProfile` 且 profile 空/null = **删除** `bps`。
- **SQL NULL 与 JSON `null` 都归一成空 envelope**（便于调用方直接 merge）。
- **canonical JSON 是语义等同的唯一判据**：键递归排序、数组保序、数字用唯一规范十进制拼写
  （`1` / `1.0` / `1e0` 等价）。`IdentityJSONEqual` 走它，**不要**用 `bytes.Equal` 直接比原始字节。
- **`CanonicalJSON` 引擎的最终归属是已知遗留问题**：本包是它当前的 owner（三个 helper 零外部消费者，
  不值得单开包）。若将来有第二方需要，再抽 `internal/canonicaljson`。
- **错误前缀是 `identityenv:`**（不是 `domain:`）。双层前缀（`domain: … : identityenv: …`）是正常现象，
  不要为消前缀去拆 `%w` 链（会破坏 `errors.Is/As`）。

## 验证

```bash
go test ./internal/identityenv/ -count=1       # 无测试文件
go test ./internal/domain/ -run 'TestBPS|TestIdentity' -count=1
```
