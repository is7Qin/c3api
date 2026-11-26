# internal/handler/user — 用户面 `/api/user/*`

> 根 `AGENTS.md` 覆盖项目级约定；`internal/handler`（根包）= 管理面 `/api/admin/*`，
> 传输装配在 `internal/server`。

## 职责

用户面路由实现（`router.go` 装配）：register / login 公开，其余 `RequireJWT`。

一资源一文件：`keys.go`（客户端 key）/ `usage.go` / `stats.go` / `orders.go` / `groups.go` /
`redemptions.go` / `management_keys.go` / `temp_balances.go` / `payment_methods.go` / `models.go` /
`errlogs.go` / `features.go` / `custom_pages.go` / `balance_warning.go` / `auth.go` / `convert.go` /
`handler.go`。端口与测试夹具在 `rule_port.go` / `rule_port_test.go`。

`api.gen.go` 是 **oapi-codegen 生成物，禁止手改**（见 `internal/handler/generate.go`）。

## 不变量

- **生成物不手改**：改 API 走 `openapi/openapi.yaml` → `go generate` → 实现 → 前端 `pnpm gen:api`。
- **handler 只做 JSON 解码与类型转换**，业务校验在 `service`（写面唯一校验权威）。
  重复判定只可能与 service 分歧。
- **`supplier` 角色不得访问 `/api/admin/*`**（见 `internal/AGENTS.md` 角色模型）。
- **本包不 import `internal/rule`**（P6-3 已断链，`productionLedger` 那条债已删）。
  `err_logs` 行级脱敏走本包自己声明的端口 `ErrLogClassifier`（`rule_port.go`）：

  ```go
  ClassifyErrLog(ctx context.Context, in ErrLogQuery) (msg string, ok bool)
  ```

  端口签名里**不出现 `rule.*`**——`ErrLogQuery` 只含用户面真有的标量
  （`AccountID/TemplateID/GroupID/Model/Kind/HTTPStatus/ErrorMessage`），`Kind` 是
  `domain.RuleKind`。实现是组合根注入的薄适配器（`cmd/server/errlog_classifier.go`
  的 `errLogClassifier`，把 `ErrLogQuery` 翻成 `rule.Event` 再调 `Classify`）。
  ⚠ 同理**不要**把 `rule.Event` 整个搬进本包：它还含 `RouteClassID` /
  `QualityClassID` / `CandidateFingerprint` / `ExpectedIdentityRevision` 等执行面
  身份字段，用户面语义无关。
- **`h.rules != nil` 的 nil 语义是「端口缺席 ⇒ 不脱敏」**（`convert.go` 的
  `toAPIErrLog`）：组合根恒装配，nil 只出现在测试里。⚠ 这个降级**不会**让任何测试
  变红——漏接适配器的后果是用户看到平台内部错误原文。装配点由
  `cmd/server/errlog_classifier_wiring_test.go` 的 AST 门禁钉死，改装配必须让那条绿。
- `unifiedMessage`（本包私有）与 `internal/rule.UnifiedMessage` 是**同一个公式的
  两份实现**：`msg = CustomMessage != nil ? *CustomMessage : upstream`。私有实现
  是为不 import 引擎，所以"改一处必须改另一处"由
  `rule_port_test.go` 的 `TestUnifiedMessageMatchesRule` 对照钉住。⚠ 两者的第二个
  参数（上游正文）**都未被使用**——不要"顺手"用上，那会与 rule 版分叉。
- 本包的**同包测试**仍 import `internal/rule`（`ruleEngineClassifier` 夹具，在
  `rule_port_test.go`）：`convert_test` / `errlog_status_boundary_test` 必须装
  **真实**引擎才能证明「种子规则/透传规则真的生效」「`status_code=0 ⇒ 带
  http_status 的规则不匹配」。这是登记在 `internal/architecture` **`testLedger`**
  的夹具需求，不是生产边；换成 fake 就变成测替身。

## 验证

```bash
go test ./internal/handler/user/ -count=1
```
