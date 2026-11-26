# internal/modelmap — 模板 model_mapping 的值类型与校验（中立叶子包）

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

`ent` 的生成模型要表达 `model_mapping` 这个 JSON 列（`Template.ModelMapping`），而 `ent` 不能反向
依赖 `internal/domain` 值类型层。把这类"纯序列化契约"下沉成叶子包后，`ent` 与 `domain` 各取所需，
`domain` 侧留**同名别名**（`type ModelMapping = modelmap.ModelMapping`）⇒ 120+ 个消费者零改动。

## 硬边界

只 import stdlib（`bytes` / `encoding/json` / `fmt` / `strings`）。**不得** import 任何 `internal/*`
或 `pkg/*`。allow 矩阵里本包规则是显式的 `to: []string{}`。

## 导出面

`ModelMapping`（`map[string]ModelMappingEntry`）/ `ModelMappingEntry`（`MappedModel` + `Mode`）/
`ModelMappingMode`（`Invalid` / `Explicit` / `Implicit` + `Valid`/`String`/JSON 编解码）/
`ValidateModelMapping`。

## 不变量

- **JSON 严格性**：`ModelMapping.UnmarshalJSON` 拒绝 `null`；`ModelMappingEntry.UnmarshalJSON` 要求
  `mapped_model` 与 `mode` **都存在**且**无多余字段**（`additional properties not allowed`）。
- **`mapped_model` 非空且无首尾空白**；alias 同理。
- **12 条错误文案有门禁**：`internal/domain/model_mapping_literals_test.go` 会扫 `modelmap.go` 的每个
  `fmt.Errorf` 字面量并要求在 `mappingErrorLiterals` 里登记（双向对账）。**加文案必须同步登记**，
  否则红；登记了但源码没了也红。

## 验证

```bash
go test ./internal/modelmap/ -count=1          # 无测试文件
go test ./internal/domain/ -run TestMappingErrorLiteralsHaveAssertions -count=1   # 文案门禁
```
