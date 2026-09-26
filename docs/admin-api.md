# Admin API 文档

管理端 API（配置模板 / 账号 / 分组 / 兑换码 / 模型价格 / 日志 / 统计 / 临时额度）。与 AI 推理请求（`/v1/*`，模型请求）相对，本组接口为网关管理面，不对上游转发。

## 通用约定

- **Base URL**：`http://<gateway>/api/admin`
- **认证**：两条路径任一通过即可。静态 admin token（`Authorization: Bearer <admin_token>`，`config.toml` 的 `admin.token` / 环境变量 `C3API_ADMIN_TOKEN`；**可选项**，空 = 不启用静态 token 鉴权）或 platform_admin JWT（与 `/api/user` 面同签发，快照角色覆盖 claims）。两者皆缺失/错误、或普通 `user` 角色 JWT → `401`（详见「鉴权与 created_by 约定」）。
- **Content-Type**：请求体与响应均为 `application/json`（`rotate-key` 等无请求体操作除外）。
- **错误格式**：非 2xx 响应体为 `{"error": "<消息>"}`。404 的消息含缺失资源 id（如 `service: not found: id=999 missing`），便于定位。
- **ID**：路径参数 `{id}` 为模板/账号/分组的整数 ID。
- **更新语义**：`PUT` 为**全量替换**——请求体中的字段整体覆盖，未提供的字段清零（仅提供部分字段的 `PUT` 会把缺失字段重置为空/零值）。批量 `batch-update` 为**部分更新**（只改 `fields` 中提供的字段）。
- **列表响应**：templates / accounts / groups 三个旧端点统一返回 `{"total": <满足筛选的总数>, "rows": [...]}`，支持 `limit` / `offset` 分页、筛选参数与白名单 `sort` / `order` 排序（非法 `sort` / `order` → `400`）。兑换码与模型价格为**增强分页范式**（`page` / `page_size`，1-based），见对应章节。
- **金额单位（两个面，勿混）**：**明细/计数面恒为毫分整数**（1 USD = 100,000 毫分）——`usage_logs` 的 `Cost` / `RawCost`、价格快照 `Price*Millis`（每 M token 毫分）、key 的 `Quota` / `QuotaUsed`、`/routing/frontier` 的 `cost_per_success`；这些字段在 API 边界**原样下发，不做换算**。**聚合/账户面恒为 USD float64**——`users.balance`、`temp_balances.amount_usd` / `total_usd`、兑换码 `value`、`/stats/*` 与 `/overview` 的 `Cost` / `cost_usd` / `raw_cost_usd`；这些字段由服务端按 `毫分 / 1e5` 换算后下发。同一含义在两面的类型与量纲不同（如 `usage_logs.Cost` 是毫分 int64，`/stats/trend` 的 `Cost` 是 USD float64），客户端不得跨面直接比较或相加。

## 枚举值

| 枚举 | 取值 |
|---|---|
| `format`（请求格式） | `openai-chat` / `openai-responses` / `openai-responses-ws` / `openai-images` / `openai-search` / `anthropic` |
| `failure_source`（账号失效来源） | `rule`（规则 `fail_account` 判死）/ `sdk`（凭据 fatal 判死）；`recover` 清除 |
| 运行时健康（RuntimeHealth，非持久字段） | `READY` / `RETRY_AFTER` / `OPEN` / `PROBING`（探针单 permit）；终态 `FAILED`/`DISABLED` 来自账号生命周期，非健康机状态 |
| `error_type`（日志） | `none` / `429` / `4xx` / `5xx` / `network` / `auth` / `no_account` / `abort` / `billing`（计费拒绝 402） |
| `type`（兑换码） | `balance`（充值余额，面值 USD）/ `concurrency`（加并发数，面值整数）/ `temp_balance`（临时余额，面值 USD，兑换后资源到期） |
| `status`（兑换码） | `active` / `disabled`（不可编辑，仅可失效） |
| `source`（模型价格） | `litellm`（官方价格表拉取）/ `manual`（管理端手动设价，优先级最高） |

---

## 模板 Templates

模板定义上游厂商：base_url、支持的请求格式集合、可服务模型集合、格式级模型覆盖与模型映射。

> **破坏性变更**：`default_format` 已移除（由必填的 `supported_formats` 数组取代），`model_formats` 已移除（由反转为按格式组织的 `format_models` 取代）。`model_mapping` 已由 `map[string]string` 破坏性替换为 `map[string]{mapped_model, mode}`（见下方模型映射合同）。旧数据未迁移，使用旧字段的客户端需按下方新结构调整；本次为 Beta 破坏性变更，升级需全新部署，无迁移路径。

### 创建模板

`POST /api/admin/templates`

请求体：

```json
{
  "name": "openai-main",
  "base_url": "https://api.openai.com",
  "supported_formats": ["openai-chat", "openai-responses"],
  "models": ["gpt-4o", "gpt-4o-mini"],
  "format_models": { "openai-responses": ["gpt-4o-mini"] },
  "model_mapping": {
    "gpt-4o": { "mapped_model": "gpt-4o-2024-11-20", "mode": "explicit" },
    "gpt-4o-mini-cheap": { "mapped_model": "gpt-4o-mini", "mode": "implicit" }
  }
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 模板名 |
| `base_url` | string | ✅* | 上游地址（可带协议前缀，如 `/zen`；**不要以 `/v1` 结尾**——`/v1` 由网关按格式追加：openai 系拼 `/v1/...`，anthropic SDK 自带 `v1` 前缀；以 `/v1` 结尾会被拒（`400`）；`codex-oauth`/`codex-pat` 模板该字段须为空（SDK 默认端点，非空 → `400`），`api_key`/`responses-special` 为可选覆盖） |
| `supported_formats` | string[] | ✅ | 支持的请求格式枚举数组（至少 1 项，项枚举见上；重复/非法枚举返回 `400`） |
| `models` | string[] | 否 | 可服务模型名集合 |
| `format_models` | object | 否 | 格式级模型覆盖：`{格式: [模型名]}`，key 必须是 `supported_formats` 子集、模型必须是 `models` 子集（否则 `400`）；未配置的格式 = 全部 `models` |
| `model_mapping` | object | 否 | 模型映射：`{客户端模型别名: {mapped_model, mode}}`；每条为严格对象 `required: [mapped_model, mode]` 且 `additionalProperties: false`，`mode` 仅 `explicit`/`implicit`，缺失/null/空/未知/旧字符串一律 `400`，无默认值；别名与 `mapped_model` 大小写敏感，拒绝空/纯空白及首尾空白，UI 提交前 trim 并按 trim 后去重校验；恒等映射（`alias == mapped_model`）合法且保留其 `mode`） |

响应 `200`：创建后的模板对象（字段为大写，见下方模板对象结构）。

### 模板对象结构（响应）

```json
{
  "ID": 1,
  "Name": "openai-main",
  "BaseURL": "https://api.openai.com",
  "SupportedFormats": ["openai-chat", "openai-responses"],
  "Models": ["gpt-4o", "gpt-4o-mini"],
  "FormatModels": { "openai-responses": ["gpt-4o-mini"] },
  "ModelMapping": {
    "gpt-4o": { "mapped_model": "gpt-4o-2024-11-20", "mode": "explicit" },
    "gpt-4o-mini-cheap": { "mapped_model": "gpt-4o-mini", "mode": "implicit" }
  },
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

> 注意：响应字段为 **Go 默认大写命名**（`ID` / `Name` / `BaseURL`…），请求字段为 snake_case。服务创建或更新后的有效空 `ModelMapping` 始终按规范序列化为 `{}`，不会以 `null` 返回。创建和全量 PUT 省略 `model_mapping` 时归一化为 `{}`；显式发送 `model_mapping: null` 返回 `400`。批量更新中省略 `model_mapping` 保留原值，发送 `{}` 清空映射，显式 `null` 返回 `400`。`ModelMapping` 元素为 `{mapped_model, mode}` 严格对象。

### 模板列表

`GET /api/admin/templates`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数（≤0 视为 20） |
| `offset` | int | 0 | 分页偏移（<0 视为 0） |
| `sort` | string | `id` | 白名单：`id` / `name` / `base_url` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |

响应 `200`：

```json
{
  "total": 2,
  "rows": [
    { "ID": 1, "Name": "openai-main", "BaseURL": "https://api.openai.com", "...": "模板对象字段" }
  ]
}
```

### 模板批量操作

`POST /api/admin/templates/batch-delete`

请求体：`{"ids": [1, 2, 3]}`（1–100 条，重复 id 自动去重）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deleted": 3}`（按去重后的条数计）；事务全成或全败 |
| `400` | `ids` 为空或超过 100 条；`fields` 为空（batch-update） |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（全败，不部分删除） |

`POST /api/admin/templates/batch-update`

请求体：`{"ids": [1, 2], "fields": {"name": "renamed"}}`。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 否 | 模板名（非空） |
| `base_url` | string | 否 | 上游地址（可带协议前缀，如 `/zen`，**不要以 `/v1` 结尾**）；`codex-oauth`/`codex-pat` 模板该字段须为空（SDK 默认，非空 → `400`），`api_key`/`responses-special` 为可选覆盖（留空保持不变） |
| `supported_formats` | string[] | 否 | 支持的请求格式枚举数组（至少 1 项，枚举见上；重复/非法枚举 → `400`） |
| `models` | string[] | 否 | 可服务模型集合 |
| `format_models` | object | 否 | 格式级模型覆盖：`{格式: [模型名]}`，key 必须是 `supported_formats` 子集（同批提供时校验）、模型必须是 `models` 子集 |
| `model_mapping` | object | 否 | 模型映射：省略 = 保留不变；显式 `null` → `400`；提供对象 = 全量替换，`{}` = 清空（见批次三态）；每条同创建约束（`mapped_model`+`mode` 必填，`additionalProperties: false`，无默认值） |

`fields` 必须至少提供一字段；`ids` 中任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 2}`。

### 模型映射 Model Mapping（Beta 破坏性合同）

`model_mapping` 是模板的唯一模型映射事实源，类型为 `map[string]ModelMappingEntry`，其中 `ModelMappingEntry = {mapped_model: string, mode: explicit|implicit}`。无第二列/伴生映射，无旧 `map[string]string` 兼容解析，无默认值，无迁移。

**严格对象契约：**
- 每个条目 `required: [mapped_model, mode]` 且 `additionalProperties: false`；缺失/`null`/空字符串/未知 `mode`/旧字符串形态一律 `400`。
- `mapped_model` 与别名（key）大小写敏感；拒绝空、纯空白、首尾空白（服务层校验；UI 在提交前 trim 并按 trim 后结果拒绝重复别名）。
- 恒等映射（`alias == mapped_model`）合法，保留其显式声明的 `mode`。
- 持久化 JSON 严格解码：非法枚举、畸形对象、旧字符串、`null` 均返回仓库加载错误，不回退为 `explicit`；初始加载失败则无可用快照（fail-closed），运行时重载保留上一有效不可变快照并通过快照观测上报错误。
- 空映射规范值恒为 `{}`；`PUT`/`POST` 省略等价 `{}`, `PUT` 全量替换；批次三态见下。

**批次三态（`POST /api/admin/templates/batch-update`）：**
- `fields` 未提供 `model_mapping`（省略）→ 保留不变（不触及已有映射）；显式 `null` → `400`；
- `fields.model_mapping` 提供对象 → 全量替换为该对象所列条目；
- `fields.model_mapping: {}`（零条）→ 清空映射；
- 无按行合并（no per-row merge）。

**运行时身份矩阵：**

| 场景 | 上游请求模型 | `UsageLog.Model` | `UsageLog.MappedModel` | 计价/预检模型 | 客户端响应模型 |
|---|---|---|---|---|---|
| 无映射 | request | request | 空 | request | 透传不变 |
| explicit，目标不同 | target | request | target | target | 透传不变 |
| explicit，恒等目标 | request | request | 空 | request | 透传不变 |
| implicit，目标不同 | target | request | request（即客户端模型） | request | 已识别路径重写为 request |
| implicit，恒等目标 | request | request | request | request | 已识别路径重写为 request |

- `Selection.Model` 保持上游目标与规则/熔断身份；调度器仅一次 map 查找返回 `target+mode`，并导出 `UsageMappedModel`（精确写入 `UsageLog.MappedModel`）与 `ResponseModel`（仅 implicit 时为客户端请求模型，否则空），计价预检取 `UsageMappedModel` 非空即用之，否则取 `Selection.Model`。
- 终局日志/计费以当次选中的 `Selection` 直接为准（成功、已接受流中止、客户端 499、上游 4xx、价格预检拒绝、耗尽熔断取 `lastSel`；本地预选拒绝前无选中则空）。`Search` 全路径不受 `UsageMappedModel`/`ResponseModel` 影响，沿用 `MappedModel = mappedFor(reqModel, sel.Model)` 既有行为。

**可识别响应路径（仅重写已存在的字符串值，不创建缺失字段）：**
- `model`（OpenAI Chat/Responses 等顶层）
- `response.model`（Responses 结构）
- `message.model`（Anthropic `message` 结构）
- 每条 payload 中同时出现的多路径全部重写；`null`/数字/对象/数组/畸形 JSON/无关嵌套 `model`/`[DONE]`/注释/不透明字节一律不触及；HTTP 非 2xx 错误体不重写。

**流式语义（无全流缓冲，逐帧改写）：**
- REST 成功 = 上游返回 2xx 前的成功响应；SSE 成功 = 上游返回 `200` 后逐帧可识别数据帧实时改写，后续流中止不回滚已发出帧；WS 成功 = 上游接受首帧后逐帧可识别上游文本帧实时改写，二进制/不透明帧不改。
- **SSE 顺序**：原生 `sserelay` 调用方若同时配置 `Mapper` 与 `Observer`，`Mapper` 先执行，非丢弃输出先写入客户端，再以原始未修改事件回调 `Observer`；被丢弃帧仅回调 `Observer` 不写出，写入失败则在 `Observer` 前返回；转换流式当前无 `Observer`，TTFT 与用量提取在 `Mapper` 内以原始 `ev.Data` 于 `StreamMapper.Map` 之前发生，顺序不变。已改变的 SSE 负载保留全部非 `data:` 行（`event`/`id`/`retry`/注释及顺序），仅替换逻辑 `data`，仅在真实 implicit 改写时才可能将多 `data:` 行归一为一行。
- **WS 顺序**：上游→客户端文本帧在 `frameHook`/fatal 鉴权与用量/图片嗅探（原始字节）之后、客户端写出之前重写；显式/未映射路径不安装 mapper，不做响应扫描。
- **转换顺序**：原始上游观测 → `ConvertResponse`/`StreamMapper` 协议转换 → 最终面向客户端输出的 implicit 重写 → 客户端写出。转换流式 `StreamMapper.Map` 可能返回零/一/多完整 SSE 帧，helper 遍历整个返回切片逐帧重写，保留帧边界/顺序/元数据与 `[DONE]` 语义。
- 共享 helper 在 `override` 为空、无已识别字符串路径或全部值已相等时返回原切片；已改变 SSE 帧才重建。

**排除项（保持现有字节/行为不变）：**
- 标准 Images：JSON 请求按目标改写，multipart 保持字节一致的请求体/表单模型例外（不重建 multipart）；标准/Codex Images 响应不凭空新增 `model` 字段，无可识别模型字段则字节不变；计费仍按既有 Images 路径。
- Search：完全不透明透传（客户端请求字节直达上游，响应字节直回，固定 `codex-search` 计费与既有 `MappedModel` 日志行为），不使用 `UsageMappedModel`/`ResponseModel`。
- `/v1/models`、映射键路由别名、硬白名单成员、目标准入、调度器分层与规则语义保持不变；错误负载不重写。

> **Beta 说明**：本映射合同为 Beta 破坏性变更，无兼容/迁移/旧形态/默认值/双写/清理 shim；升级需全新部署（新库），旧库残留 `model_mapping` 字符串值在加载时直接报错。

### 模板其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /api/admin/templates/{id}` | 单个模板 | `200`：模板对象；`404` 不存在 |
| `PUT /api/admin/templates/{id}` | 全量更新（字段同创建） | `200`：更新后模板对象 |
| `DELETE /api/admin/templates/{id}` | 删除 | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id）；仍被账号引用时返回 `500`（DB 外键约束） |
| `GET /api/admin/templates/{id}/ext` | 读取模板类型化扩展（编辑回显；仅生态三类型模板有 ext 行） | `200`：ext 配置 |
| `PUT /api/admin/templates/{id}/ext` | 幂等写入模板类型化扩展（Create/Update 合一；全列更新含 NULL 清空） | `200`：写入后的 ext 配置 |

> 模板变更（含 base_url / supported_formats / format_models / model_mapping）通过 invalidate 回调即时生效于调度器快照与上游 SDK 客户端（无需重启）。

---

## 账号 Accounts

账号绑定模板并持有上游 API key，是调度的基本单元。**选号顺序不由手工权重决定**——后台 RoutingCompiler 依据持久质量统计、采购成本倍率、缓存域与运行时健康编译不可变路由计划（观测面与缓存亲和语义见「路由观测 Routing」）；账号面只提供三类事实：生命周期（`enabled` / `failed_at` / `failure_source` / `lifecycle_revision`）、采购成本（`upstream_cost_multiplier`）与缓存域（`cache_domain`）。

### 创建账号

`POST /api/admin/accounts`

```json
{
  "name": "a1",
  "template_id": 1,
  "upstream_key": "sk-xxx",
  "cache_domain": "dc-a.example",
  "max_concurrency": 8
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 账号名 |
| `template_id` | int | ✅ | 所属模板 ID |
| `base_url` | string | 否 | 账号级覆盖（可带协议前缀，如 `/zen`，不要以 `/v1` 结尾，留空继承模板）；`codex-oauth`/`codex-pat` 关联模板须为空（SDK 默认，非空 → `400`），`api_key`/`responses-special` 为可选覆盖 |
| `upstream_key` | string | ✅* | 上游 API key（`codex-oauth`/`codex-pat` 关联模板可为空，凭据走 `account_ext`；`api_key`/`responses-special` 必填） |
| `cache_domain` | string / null | 否 | 共享缓存域（软亲和一致性哈希的域标识，请求侧显式亲和键语义见「路由观测 Routing」；合法域名形态 ≤253，非法 → `400`）；创建缺省/`null` = 账号私有域；后续更新走 `PATCH /accounts/{id}`（fenced），`null` = 清空回私有域、缺席 = 不变、空串 → `400` |
| `max_concurrency` | int | 否 | 账号并发上限；创建时缺省取服务端配置 `scheduler.default_max_concurrency`（显式提供则用之）。写入期**不做静默钳制**——`0` 会让该账号恒不可被选中，属误配置 |
| `group_ids` | int[] / null | 否 | 所属分组；缺省/`null`/`[]` = 不归组（不归组的账号不进任何组路由，但账号本身可被直接管理） |
| `enabled` | bool | 否 | 管理端启停；创建缺省 `true`，**显式 `false` 生效**（创建即为禁用态） |
| `upstream_cost_multiplier` | number / null | 否 | 采购成本倍率；创建缺省 ×1（存储 `10000` bp），**显式 `0` 生效**（创建即为免费账号，不被缺省 ×1 吞掉）。正常值 `1` = ×1、`0` = 免费、上限 `10` = ×10；边界换算 basis points（存储 `25000` ↔ 显示 `2.5`），越界 → `400`。与租户/组计费倍率（`price_multiplier`）完全独立，永不互串 |

创建体是**同一个字段模型** `AccountConfigPatch` 加上必需性投影（`required[name, template_id]`），故上表与 `PATCH /accounts/{id}` 的字段逐一对应；三态规则（可空标量 `null` = 清空、缺席 = 不变、不可空标量 `null` 或可空标量 `""` → `400`）对创建与 `PATCH` 一致。

响应 `200`：账号对象（含嵌套 `Template`）。

### 账号对象结构（响应）

```json
{
  "ID": 1,
  "Name": "a1",
  "TemplateID": 1,
  "Template": { "...": "嵌套模板对象" },
  "UpstreamKey": "sk-xxx",
  "MaxConcurrency": 8,
  "Enabled": true,
  "FailedAt": null,
  "FailureSource": null,
  "LastError": null,
  "LifecycleRevision": 1,
  "UpstreamCostMultiplier": 1.0,
  "CacheDomain": "dc-a.example",
  "LastUsedAt": null,
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

### 生命周期模型（两轴正交）

| 轴 | 字段 | 语义 |
|---|---|---|
| 启用轴 | `enabled` | 管理端启停（`PATCH /accounts/{id}`，fenced）。禁用 = 不参与选号；**enable 不清失效字段** |
| 失效轴 | `failed_at` + `failure_source`（`rule`/`sdk`）+ `last_error` | 运行时终态判死（规则 `fail_account` 命中 / SDK 凭据 fatal）；恢复唯一入口 `POST /accounts/{id}/recover` |

- 两轴独立：重新启用不清失效，清除失效不覆盖管理禁用。`failed_at != null` 或 `enabled = false` 的账号不进入任何路由计划热车道，也不被自动探索复活。
- 两个代际分开：`lifecycle_revision`（C，客户端 CAS 令牌）**每次写入** +1；`identity_revision`（K，身份代际）仅在管理面**身份类写入**真的改变取值时 +1。所有 fenced 端点必须携带 `expected_revision`（即 C），过期 → `409`（重读账号后重试）。在途判定（失效判决 / latch / 健康记录 / continuation 绑定）一律围栏在 (I, K) 上、**不**看 C；SDK 内部 OAuth 自动轮转两个代际都不动——既不失效在途计划，也不失效在途 continuation/fatal fence。
- 瞬时健康（429 节流 / 短时摘除）**不落账号行**——RuntimeHealth（Redis/本地）态，随代际过期。
- **失效恢复须知（SDK 接入账号——codex-oauth / codex-pat）**：凭据被判死后 `/recover` 只清失效字段并置 PROBING（探针环接管，READY 前不吃正常流量）——若凭据确已判死，请求面仍恒失败并再次判死。**恢复须先重新导入凭据**（`PUT /api/admin/accounts/{id}/ext`，凭据值真变 → 适配层重建 + 身份代际 K +1）才可真正服务——判死凭据不得在未重新导入的情况下复活。

### fenced 写端点

| 方法/路径 | 请求体 | 语义 |
|---|---|---|
| `PATCH /api/admin/accounts/{id}` | `AccountConfigPatch`（+ 可选 `If-Match: <C>`） | 账号配置的**唯一**写面：一次调用原子更新 `name` / `template_id` / `base_url` / `upstream_key` / `max_concurrency` / `group_ids` / `enabled` / `cache_domain` / `upstream_cost_multiplier`。三态：可空标量 `null` = 清空、缺席 = 不变、不可空标量 `null` 或可空标量 `""` → `400`；`group_ids: []` = 清空、`group_ids: null` = 不变。`If-Match` 陈旧 → `412`、语法非法（`W/` / 多值 / `*` / 非整数）→ `400`、缺席 = 无条件生效 |
| `POST /api/admin/accounts/{id}/recover` | `{expected_revision}` | 失效恢复唯一入口：清 `failed_at`/`failure_source`/`last_error` + C +1（K 不变）→ 显式释放 latch + 按账号清健康记录（含 Redis 墓碑）→ 新代际 PROBING。**不切 `enabled`** |

成功均 `200` 回显新代际账号对象；`404` 账号不存在；`409` 代际过期。

### 账号列表（含运行时视图）

`GET /api/admin/accounts`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数 |
| `offset` | int | 0 | 分页偏移 |
| `sort` | string | `id` | 白名单：`id` / `name` / `template_id` / `max_concurrency` / `last_used_at` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |
| `template_id` | int | — | 按所属模板过滤 |

响应 `200`：`{"total": <总数>, "rows": [...]}`。每个元素为账号对象 + 三个运行时字段（来自调度器内存快照）：

```json
{
  "total": 1,
  "rows": [
    {
      "ID": 1,
      "Name": "a1",
      "...": "账号对象字段",
      "concurrency": 3,
      "err_rate": 0.05,
      "err_count": 2
    }
  ]
}
```

| 运行时字段 | 说明 |
|---|---|
| `concurrency` | 当前在途并发数（内存计数） |
| `err_rate` | 错误率 EWMA（0.0–1.0，定点 1e6 缩放后输出） |
| `err_count` | 连续错误计数（决定退避指数） |

### 账号批量操作

`POST /api/admin/accounts/batch-delete`

请求体：`{"ids": [1, 2]}`（1–100 条，重复自动去重）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deleted": 2}`（按去重后的条数计）；事务全成或全败 |
| `400` | `ids` 为空或超过 100 条 |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（全败） |

`POST /api/admin/accounts/batch-update`

请求体：`{"ids": [1], "fields": {"max_concurrency": 16}}`。

`fields` 用的是**同一个字段模型** `AccountConfigPatch`（与 `PATCH /accounts/{id}` 逐字段同语义、含三态规则）——即批量面能改 `PATCH` 能改的全部字段：`name` / `template_id` / `base_url` / `upstream_key` / `max_concurrency` / `group_ids` / `enabled` / `cache_domain` / `upstream_cost_multiplier`。逐字段的类型与约束见上文「创建账号」的字段表（`base_url` 按**合并后**的模板凭据类型判定、`upstream_key` 按**合并后**的模板类型判定等）。

`fields` 必须至少提供一字段；`ids` 中任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": N, "items": [{"account_id": 1, "lifecycle_revision": 2}, ...]}`——**逐账号**回显新 C，供客户端下一次 CAS。批量面不携带逐账号前置条件（无 `If-Match`），但代际仍逐账号原子 +1；批量改 `group_ids` 时旧组与新组都会被刷新（旧∪新）。

### codex 凭据批量导入

`POST /api/admin/accounts/batch-import-codex-oauth`

`POST /api/admin/accounts/batch-import-codex-pat`

把 codex 凭据批量灌入账号表。**幂等组合键 = `codex_email` + `codex_account_id`**：键已存在 → 只更新凭据列（计入 `updated`），不存在 → 新建账号（计入 `imported`）。**行级失败不毁整批**——有失败行仍返回 `200`，明细在 `failed[]`（`index` = `items` 原始下标）。

请求体（两端点同构，只有 `items[]` 的元素类型不同）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `items` | array | ✅ | 1–100 **原始条数**（空/超限 → `400`） |
| `template_id` | int | ✅ | codex 账号归属模板；缺失 → `400`，不存在 → `404`；**模板 `credential_type` 必须 == 端点类型**（`codex-oauth` / `codex-pat`），错配 → `400` 整批拒绝 |
| `group_id` | int / null | 否 | 新建账号归组；缺省 = 不归组；分组不存在 → **行级 failed**（FK 违反不整批 `400`） |
| `enabled` | bool | 否 | 新建账号的管理面启停（缺省 `true`）——**仅新建行生效** |
| `cache_domain` | string | 否 | 新建账号的共享缓存域（缺省/空串 = 账号私有域；非空须为合法小写域名 ≤253，非法 → `400` 整批拒绝）——**仅新建行生效** |
| `upstream_cost_multiplier` | number | 否 | 新建账号的采购成本倍率（缺省 ×1；`0`–`10`，`0` = 免费；越界 → `400` 整批拒绝）——**仅新建行生效** |

> **「仅新建行生效」语义**：`enabled` / `cache_domain` / `upstream_cost_multiplier` 是 **body 级**字段（不在 `items[]` 元素内），只作用于本次**新建**的账号。命中幂等键的已存在行**只更新凭据**，这三项配置不被覆盖（与 `max_concurrency` 同款"仅创建生效"语义）。三项缺省 = 走创建默认（`enabled=true` / 私有域 / ×1）。配置非法是**整批** `400`（配置对全批生效、无行级归属），与模板类型错配同级；而 `group_id` 不存在是**行级** failed——两者不要混。

`items[]` 元素（codex-oauth）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `codex_email` | string | ✅ | 组合幂等键①（codex 登录邮箱）；格式非法 → **行级 failed** |
| `codex_account_id` | string | ✅ | 组合幂等键②（上游账号/空间标识） |
| `codex_oauth_token` | string | ✅ | oauth 访问令牌（与 refresh 成对） |
| `codex_oauth_refresh_token` | string | ✅ | oauth 刷新令牌（与 token 成对） |
| `codex_oauth_expires_at` | string | 否 | RFC3339 过期时间；**解析失败 → 行级 failed**（契约里是原始字符串而非 `date-time`，故不进整批 `400`）；缺省 = 过期未知 → `401` 自愈 |
| `max_concurrency` | int | 否 | 缺省 `25`（**导入面裁决，非账号表默认 8**）；`< 1` → 归 25 |

`items[]` 元素（codex-pat）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `codex_email` | string | ✅ | 组合幂等键①；格式非法 → 行级 failed |
| `codex_account_id` | string | ✅ | 组合幂等键②（缺省时服务端按 `codex_pat_key` 在线 whoami 补全；仍空 → 行级 failed） |
| `codex_pat_key` | string | ✅ | pat（必填非空） |
| `max_concurrency` | int | 否 | 缺省 `25`；`< 1` → 归 25 |

响应 `200`：

```json
{ "imported": 3, "updated": 1, "failed": [{ "index": 2, "error": "..." }] }
```

| 字段 | 说明 |
|---|---|
| `imported` | 新建账号数 |
| `updated` | 键已存在、仅更新凭据数 |
| `failed` | 行级失败明细（`index` + `error`）；**整批不原子**，成功行已落库 |

> 批末一次性触发 invalidate + publish（`imported` 行归组 ∪ `updated` 行既有分组）——新凭据经 `account_ext` 快照即时生效，无需重启。

### 账号用量聚合

`GET /api/admin/accounts/usage?account_ids=1,2,3&from=...&to=...`

一次取多个账号的用量视图（前端账号列表免逐账号轮询）。

| 查询参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `account_ids` | string | ✅ | 逗号分隔账号 id（1–100 条，重复自动去重；空/非数字/超限 → `400`） |
| `from` / `to` | RFC3339 | 与 `window` 恰择一 | 绝对窗口。显式值为绝对时刻，不受时区改写；`from` 不早于 `to` → `400` `reason=window_invalid`。与 `window` 同给、只给一端、或两者都不给 → `400` `reason=window_ambiguous`。**没有「缺省 = 当天」** |
| `window` | string | 与 `from`/`to` 恰择一 | 相对窗口，只收 Go `time.ParseDuration` 时长串（`24h` / `168h` / `720h` / `2160h`）。服务端自持时钟：`to` = 当前时刻向上取整到 UTC 整点、`from` = `to` − `window`，两端恒整点。`7d` 不是时长单位 → `400` `reason=window_invalid` |
| `timezone` | string | 否 | 非法 IANA 名 → `400`。不再决定任何缺省窗口 |

响应 `200`：`{"items": [...]}`，`items` **恒 = `account_ids` 去重后的全量**（无记录账号 gateway 全 0，前端免补零；顺序同去重后顺序）。

每个 item：

| 字段 | 类型 | 说明 |
|---|---|---|
| `account_id` | int64 | 账号 id |
| `gateway` | object | 网关侧聚合（`usage_logs` 实时明细）：`request_count` / `error_count` / token 计数 / `cost_usd` / `raw_cost_usd`（**USD float64**，毫分 /1e5）；无记录账号全 0 |
| `upstream` | object / null | **codex 上游额度快照**（上游账号的用量与重置时间，百分比口径）——**不是本网关的计费**；`api-key` / 无凭据账号恒 `null` |
| `upstream_error` | string / null | 上游快照失败分类：`auth_expired`（凭据 fatal）/ `upstream_unavailable`（网络/5xx）；`null` = 无上游能力或快照成功 |

### 账号其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /api/admin/accounts/{id}` | 单个账号 | `200`：账号对象；`404` 不存在 |
| `PATCH /api/admin/accounts/{id}` | 部分更新（唯一字段模型 `AccountConfigPatch`，三态语义；可选 `If-Match` 前置条件） | `200`：更新后账号对象；陈旧 → `412` |
| `DELETE /api/admin/accounts/{id}` | 删除 | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id） |
| `GET /api/admin/accounts/{id}/groups` | 读取账号的全部分组 id（编辑回显；不随账号列表返回） | `200`：`{"group_ids": [...]}` |
| `GET /api/admin/accounts/{id}/ext` | 读取账号类型化鉴权扩展（编辑回显；仅 `codex-oauth` / `codex-pat` 账号有 ext 行） | `200`：ext 配置 |
| `PUT /api/admin/accounts/{id}/ext` | 幂等写入账号类型化鉴权扩展（Create/Update 合一；全列更新含 NULL 清空）；凭据值真变 → 适配层重建 + 身份代际 K +1 | `200`：写入后的 ext 配置 |

---

## 分组 Groups

分组持有客户端 key（`ck-` 前缀），N:M 绑定账号。AI 请求以分组 key 鉴权，请求在组内账号中调度。

### 创建分组

`POST /api/admin/groups`

```json
{ "name": "bench", "price_multiplier": 2.0 }
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 分组名（唯一） |
| `price_multiplier` | number / null | 否 | **价格倍率**（正常值：`1` = ×1、`0` = 免费、上限 `10` = ×10；API 边界与万分数换算——内部存储恒万分数）。缺省/`null` = 不设置（×1）；**显式 `0` = 免费组（创建路径即可设置）**。超界 → `400` |

响应 `200`：创建后的分组对象：

```json
{
  "ID": 1,
  "Name": "bench",
  "Visibility": "public",
  "PriceMultiplier": 2.0,
  "CreatedAt": "2026-08-09T10:00:00+08:00",
  "UpdatedAt": "2026-08-09T10:00:00+08:00"
}
```

### 分组对象结构（响应）

| 字段 | 类型 | 说明 |
|---|---|---|
| `ID` | int64 | 分组 id |
| `Name` | string | 分组名 |
| `Visibility` | `public` / `private` | public 全部用户可选；private 仅授予用户（`/api/admin/groups/{id}/assignments`） |
| `PriceMultiplier` | number（float64） | **价格倍率**（正常值，见上）；计费按 `用户-组专属倍率 ?? 组倍率 ?? ×1` 生效（见「价格倍率语义」章节） |

### 分组列表

`GET /api/admin/groups`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数 |
| `offset` | int | 0 | 分页偏移 |
| `sort` | string | `id` | 白名单：`id` / `name` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |

响应 `200`：`{"total": <总数>, "rows": [分组对象...]}`（不含明文 key）。

### 分组批量操作

`POST /api/admin/groups/batch-delete`

请求体：`{"ids": [1, 2]}`（1–100 条，重复自动去重）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deleted": 2}`；事务全成或全败；删除前先清理各组注册 key |
| `400` | `ids` 为空或超过 100 条 |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（全败） |

`POST /api/admin/groups/batch-update`

请求体：`{"ids": [1], "fields": {"name": "renamed"}}`（`name` 非空，`fields` 必须提供）。任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 1}`。

### 分组其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /api/admin/groups/{id}` | 单个分组 | `200`：分组对象 |
| `PUT /api/admin/groups/{id}` | 全量更新分组（`name` / `visibility` / `price_multiplier`） | `200`：更新后分组对象；`price_multiplier` 缺省 = 保持原值、显式提供（含 `0` = 免费）即写入 |
| `DELETE /api/admin/groups/{id}` | 删除（先删注册 key 再删 DB） | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id） |
| `PUT /api/admin/groups/{id}/assignments` | 设置组的授予用户（替换语义）+ 用户-组专属倍率 | `200`：`{"user_ids": [...], "multipliers": {...}}`；见下方 |
| `PUT /api/admin/groups/{id}/accounts` | 绑定账号集合 | 请求体 `{"account_ids": [1, 2, 3]}`；`200`：`{"updated": true}` |
| `POST /api/admin/groups/{id}/rotate-key` | 轮换分组 key | `200`：`{"key": "ck-<新明文>"}`（旧 key 立即失效） |

> `setGroupAccounts` 为**全量替换**绑定关系（传空数组清空）。变更即时触发调度器快照重建（invalidate）。

### 设置组授予用户 + 用户-组专属倍率

`PUT /api/admin/groups/{id}/assignments`（platform_admin 专属；替换语义：`user_ids` 未列出即撤销，空数组 = 清空）

```json
{
  "user_ids": [3, 7],
  "multipliers": { "3": 2.0, "7": null }
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `user_ids` | int64[] | 完整授予列表（未列出即撤销） |
| `multipliers` | object | 可选：`user_id` → 该用户在该组的**专属价格倍率**（正常值 `0`~`10`；`null` = 清除为未设置 → 回退组倍率）。仅对 `user_ids` 中列出的用户生效；未列出的用户沿用当前值；key 必须 ⊆ `user_ids`（否则 `400`） |

响应 `200`：`{"user_ids": [...], "multipliers": {"3": 2.0, "7": null}}`（`multipliers` 为该组各授予用户的 post-state 专属倍率，`null`/缺省 = 未设置 → 用组倍率）。变更触发余额倍率快照定向刷新（invalidate）。

---

## 用户 Users

用户是鉴权与计费的顶层实体（标识 = 邮箱）。**余额字段在 API 边界统一换算 USD float64**——内部存储恒为毫分（1 USD = 100,000 毫分 = 10⁻⁵ USD 精度，扣费零换算零取整误差）；输入 `math.Round(usd × 1e5)`、展示 `毫分 / 1e5`（如 `1.5` = $1.50 = 150,000 毫分）。

### 创建用户

`POST /api/admin/users`（platform_admin 专属）

```json
{
  "email": "alice@example.com",
  "password": "s3cret-pass",
  "max_concurrency": 4,
  "balance": 10
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `email` | string | ✅ | 邮箱（唯一/格式校验） |
| `password` | string | ✅ | bcrypt 散列存储；≤ 72 字节 |
| `role` | `platform_admin` / `user` | 否 | 缺省 `user` |
| `status` | `active` / `disabled` | 否 | 缺省 `active` |
| `max_concurrency` | int | 否 | 用户级在途上限；0 = 不限 |
| `balance` | number（USD） | 否 | 余额 USD float64（≥ 0；`10` = $10 = 1,000,000 毫分） |

> **价格倍率按组**：用户本体无倍率字段——专属倍率挂在该用户与组的授予关系上（`PUT /api/admin/groups/{id}/assignments` 的 `multipliers`），用户在不同组可有不同倍率。

### 用户列表

`GET /api/admin/users?limit=20&offset=0&email=alice&sort=id&order=desc`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` / `offset` | int | 20 / 0 | 分页 |
| `email` | string | — | 邮箱模糊匹配 |
| `sort` / `order` | string | `id` / `desc` | 白名单排序；非法 → `400` |

响应 `200`：`{"total": N, "rows": [用户对象...]}`。**`PasswordHash` 永不下发**；`Balance` 为 USD float64；用户对象无倍率字段（倍率按组经 assignments 管理）。

### 更新用户

`PUT /api/admin/users/{id}`

```json
{ "balance": 5.25 }
```

| 字段 | 说明 |
|---|---|
| `role` / `status` / `max_concurrency` | 同创建；缺省 = 不变 |
| `balance` | USD float64（≥ 0）；缺省 = 不变 |

变更即时生效（鉴权/余额快照刷新，计费预检读内存快照）。错误映射：email 重复 → `409`；非法输入（格式/负余额）→ `400`；用户不存在 → `404`。

### 用户面

- `POST /user/auth/register` / `POST /user/auth/login`：注册（受 `signup_enabled` 设置；开启注册邮箱验证后还需先经 `/user/auth/register-code` 获取并携带 `code` 字段）与登录，返回 JWT + 用户对象（`Balance` 同样 USD float64）。
- `GET /user/auth/me`：当前用户信息。
- `POST /user/auth/change-password`：修改密码（旧密码校验复用登录语义——失败 `401` 同登录文案防枚举；新密码非空且 ≤72 字节，非法 `400`；**不撤销既有 JWT**，新密码下次登录生效），见下方「用户面：修改密码」。
- `POST /user/auth/register-code`：发送注册验证码（受 `mail.register_verification` + `signup_enabled` 双闸；未开验证 → `400` 哨兵文案；同邮箱 `60s` 限频 → `429`；已注册邮箱静默抑制发送仍返回 `{sent:true}`——防枚举）。响应恒 `{"sent":true}`。
- `POST /user/auth/forgot-password`：忘记密码发码。**恒 `200 {"sent":true}` 同形响应（防枚举）**——无论账号是否存在、邮件是否启用；实际发送条件 = `mail.enabled` 且账号存在且未限频。
- `POST /user/auth/reset-password {email, code, new_password}`：凭邮件验证码重置密码（码一次性、10 分钟有效、5 次尝试上限后须重新请求；新密码校验前置）；**不撤销既有 JWT**（同修改密码语义），新密码下次登录生效。
- `GET /user/stats`：我的用量统计（强制 `user_id` = 当前用户，防越权；返回 `StatTrendPoint[]`，与 `/api/admin/stats/trend` 同契约，见「查询用量统计」章节）。另有 `GET /user/stats/ttft`（我的 TTFT 摘要）。
- `GET|POST|PUT|DELETE /api/user/keys*`：我的密钥（列表/创建/更新/删除/轮换），含 `quota` 额度语义，见「密钥 Keys」章节。
- `GET /user/temp-balances`：我的临时额度（仅有效额度：未过期且正余额，`expires_at` 升序 FEFO 同序、永久最后；`total_usd` 合计 USD），见「临时额度 Temp Balances」章节。
- `GET|PUT /api/user/balance-warning-threshold`：读取或设置我的永久余额预警阈值，body/响应为 `{"balance_warning_threshold": 5}`，单位 USD；`0` 关闭，负值、非有限值和正值换算后为 `0` 均为 `400`。
- 兑换码（`/user/redemptions`）：`balance` / `temp_balance` 类型向毫分余额/临时额度充值，见「兑换码 Redemption Codes」章节。

### 用户面：修改密码

`POST /user/auth/change-password`（JWT 鉴权）

请求体：

```json
{
  "old_password": "current-pass",
  "new_password": "new-pass"
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `old_password` | string | ✅ | 旧密码；校验复用登录语义（bcrypt + 用户状态 `active`），失败 `401` 且文案与登录一致（防枚举探测） |
| `new_password` | string | ✅ | 新密码：非空且 ≤72 字节（bcrypt 截断限制，注册/建用户同款校验）；非法 → `400`（新密码校验前置，不触达旧密码判定） |

响应 `200`：`{"updated": true}`（bcrypt 重哈希落库）。

| 状态码 | 场景 |
|---|---|
| `200` | 修改成功 |
| `400` | 请求体非法 / 新密码为空或超 72 字节 |
| `401` | 无 / 非法 JWT（中间件拦截）；旧密码错误或用户非 `active`（**与登录同文案**，防枚举） |
| `404` | 用户不存在 |

> **不撤销既有 JWT**：无状态 token 无撤销机制——修改成功后已签发的 token 仍有效，新密码**下次登录**生效。

### 用户面：邮箱验证与密码重置（邮件服务）

邮件功能由运行时设置 `mail.*` 驱动（管理台「设置 → 邮件」页签配置：SMTP 主机/端口/账号/密码/发件人/TLS 策略），**`mail.enabled=false` 时零行为**——注册直通、忘记密码空转。

- **注册验证码**：开启 `mail.register_verification` 后，注册须先调 `/user/auth/register-code` 获取 6 位码（10 分钟有效、5 次尝试上限、同邮箱 60s 限频），注册请求携带 `code` 字段；验证通过方建号。首个完成验证的注册者成为 `platform_admin`。
- **密码重置**：忘记密码 → `/user/auth/forgot-password` 发码 → `/user/auth/reset-password` 凭码改密。码消费原子（防双花），重置后旧 JWT 存活至自然过期（≤24h）。
- **模板系统**：三套内置英文默认模板（注册验证码、重置密码验证码、余额预警）。验证码模板使用 `{{code}}` / `{{ttl_minutes}}` / `{{app_name}}`，`balance_warning` 使用 `{{balance}}` / `{{threshold}}`；管理台可编辑覆盖、清空正文即还原默认。
- **余额预警**：当前用户设置永久余额阈值后，永久余额在结算后恰由高于阈值变为不高于阈值时，若 `balance_warning.enabled=true` 则尽力发送邮件（best-effort，不影响结算且不保证送达）。临时额度不参与判断；同一用户和阈值 24h 冷却期内仅投递一轮（单轮最多 3 次 SMTP 重试），最终投递失败会释放占位以便后续事件重试，并非“24h 内仅试一次”。

### 邮件模板管理（platform_admin）

- `GET /api/admin/mail/templates`：列出全部用途模板（缺行自动合成内置默认，恒返回 `register_code`、`reset_code`、`balance_warning` 三条）。
- `PUT /api/admin/mail/templates/{purpose}`：body `{subject, body_text}`；`body_text` 置空 = 删行还原内置默认。非法 purpose → 非 200。
- `POST /api/admin/mail/channel-test`：body `{"email":"to@example.com"}`，用当前 SMTP 配置向指定地址发送通用测试邮件；不触发余额预警事件，不读取或改动预警阈值及冷却状态。

SMTP 连接参数（host/port/username/password/from/tls）同为运行时设置键 `mail.*`，经通用 `GET|PUT /api/admin/settings` 读写（`smtp_password` 明文回读，沿用 `upstream_key` 先例；前端以密码框呈现）。

| key | 默认 | 说明 |
|---|---|---|
| `balance_warning.enabled` | `false` | 余额预警全局开关；`false` 时不发送预警邮件 |

### 价格倍率语义（计费生效）

计费倍率作用在**整单计费成本**上（含 fast 倍率之后）：`cost = round(cost × mult / 10000)`（round-half-up），取数顺序为**用户-组专属覆盖组**（专属倍率按组挂载）：

1. `group_assignments.price_multiplier` 已设置（非 null，该用户在该组）→ 用户-组专属倍率；
2. 否则 `groups.price_multiplier`（组默认 `10000` = ×1）；
3. 两者均未设置 → ×1 原价。

`0` = **免费**（cost = 0 不扣费；请求仍须有价格，否则 402）；上限 `100000` = ×10。倍率预检：免费用户/组余额为 0 不 402。

---

## 密钥 Keys

key 是 AI 请求（`/v1/*`）的鉴权凭证，归属一个用户与一个分组，前缀 `ck-`。**两个面分明**：用户面（`/api/user/keys`，本人可看明文、可轮换）与管理面（`/api/admin/keys`，**永不下发明文**，仅供审计/排障）。

### 额度模型（quota）

| 字段 | 类型 | 说明 |
|---|---|---|
| `Quota` | int64 | **累计最终计费金额上限，毫分**（1 USD = 100,000 毫分）；`0` = **不限**（且此时 `QuotaUsed` 恒 `0`） |
| `QuotaUsed` | int64 | **已消耗计费金额，毫分**（后扣：请求完成后由 Recorder 周期增量回写，DB 滞后 ≤ `quota_flush_interval`） |

- **上限**：`quota` ∈ `[0, 9007199254740991]`（= JavaScript `Number.MAX_SAFE_INTEGER`，2^53−1）——上限按 UI 承载能力裁定，负数或超上限 → `400`。
- **`quota = 0` 会同时清零 `quota_used`**：显式把额度设为 `0`（= 不限）时，服务端在同一个 UPDATE 里把累计消耗一并归零。两条理由：① 不限额度下"已用"没有意义，留着只会让 UI 显示"已用 $X / 不限"；② 它是**累计**上限，不清零则历史消耗会带进下一次设额（用户刚设好额度就被旧消耗挡住）。
- **`quota_used` 的唯一服务端写点是额度更新路径**；与 Recorder 增量回写的交错是安全的——增量回写 SQL 带 `WHERE "quota" > 0` 守卫（无额度 key 恒不累计），行锁串行化下"回写先 / 清零先"两种提交顺序都收敛到 `0`，迟到的回写不会把已用量带回来。
- 额度是**软门禁**：请求热路径读内存预算快照，耗尽才回 DB 复核（见「计费 Billing」）。

### 管理面：密钥列表

`GET /api/admin/keys`（platform_admin 专属）

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `name` | string | — | 名称模糊搜索（`ILIKE '%name%'`） |
| `user_id` | int64 | — | 按归属用户收窄；缺省 = 全部用户 |
| `group_id` | int64 | — | 按归属分组收窄；缺省 = 全部分组 |
| `limit` / `offset` | int | 20 / 0 | 分页（`limit` 上限 200，超限裁剪到 200） |
| `sort` | string | `id` | 白名单：`id` / `name` / `created_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |

响应 `200`：`{"total": N, "rows": [AdminKey...]}`。**脱敏——响应不含 key 明文**（密钥明文绝不下发管理端）。

`AdminKey` 字段：`ID` / `UserID` / `GroupID` / `Name` / `Status`（`active` / `disabled`）/ `MaxConcurrency` / `Quota`（毫分）/ `QuotaUsed`（毫分）/ `CreatedAt` / `UpdatedAt` / `DeletedAt`（软删除时间戳，`null` = 存活）。

### 用户面：我的密钥

| 方法/路径 | 说明 |
|---|---|
| `GET /api/user/keys` | 我的 key 列表（`limit` 上限 200 / `offset` / `name` / `sort` / `order`）；**key 明文长期可查看/复制** |
| `POST /api/user/keys` | 创建 key（body：`name`✅ / `group_id`✅ / `max_concurrency`（`0` = 不限）/ `quota`（毫分，`0` = 不限））；组须为 `public` 或已授予本人的 `private`，否则 `400` |
| `GET /api/user/keys/{id}` | key 详情（仅本人；他人 key → `404`） |
| `PUT /api/user/keys/{id}` | 更新（`name` / `status` / `max_concurrency` / `quota`；**字段缺省 = 不变**，`nil` 不落值）；仅本人 |
| `DELETE /api/user/keys/{id}` | 删除（软删除；Auth 快照增量移除——**立即失效**） |
| `POST /api/user/keys/{id}/rotate` | 轮换明文（新明文生效，旧 key 立即失效）；仅本人 |

> 用户面 `Key` 比管理面 `AdminKey` 多一个 `key` 字段（明文）。`PUT /api/user/keys/{id}` 传 `"quota": 0` 时按上文语义**同时清零 `quota_used`**。

---

## 临时额度 Temp Balances

临时额度（`temp_balances` 表）是与 `users.balance` 分离的**限时消费额度**：扣费先扣临时额度（FEFO——最早到期先扣，永久最后），剩余再扣余额（见「计费 Billing」章节）。来源两路：注册赠金（`signup bonus`，受 `default_user_temp_balance` / `default_user_temp_balance_ttl_days` 设置）与兑换码（`redemption code`，`type=temp_balance`）。金额内部恒为**毫分整数**存储，API 边界统一换算 USD float64（`毫分 / 1e5`，1 USD = 100,000 毫分）——与 `users.balance` 同构。两个视角分明：**管理面全量**（含过期/用尽/负扣减行），**用户面仅有效额度**。

### 管理面：临时额度列表

`GET /api/admin/temp-balances`（platform_admin 专属）

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 页码，**1-based**；缺省或 `< 1` 按 1（不报错） |
| `page_size` | int | 20 | 每页行数；越界（`< 1` 或 `> 1000`）→ `400` |
| `user_id` | int | — | 按用户筛选；缺省 = 全部用户 |
| `sort` | string | `expires_at` | 白名单：`expires_at` / `amount` / `created_at`；非法 → `400` |
| `order` | string | `asc` | `asc` / `desc`；其他值 → `400` |

默认排序 `expires_at asc`（**FEFO 同序**——与扣费顺序一致；管理列表惯例默认 `desc`，本端点显式归一为 `asc`）。**全量视角**：无有效过滤——含过期 / 用尽 / 负扣减行（与用户侧"仅有效额度"分明，管理需要历史与状态全量视角）。

响应 `200`：

```json
{
  "total": 2,
  "rows": [
    { "id": 1, "user_id": 7, "amount_usd": 1.5, "expires_at": "2026-09-14T10:00:00Z", "note": "signup bonus", "created_at": "2026-08-15T10:00:00Z" },
    { "id": 2, "user_id": 7, "amount_usd": 5, "expires_at": null, "note": "redemption code", "created_at": "2026-08-15T11:00:00Z" }
  ]
}
```

`amount_usd` 为 USD float64（内部毫分 /1e5）；`expires_at` 为 `null` = **永久额度**；`note` 为固定系统备注（`signup bonus` / `redemption code`），无敏感信息。

| 状态码 | 场景 |
|---|---|
| `200` | 分页列表（增强分页范式，与兑换码/模型价格同款） |
| `400` | 非法 `sort` / `order` / `page_size` 越界 |
| `401` | admin 凭据（静态 token 或 platform_admin JWT）缺失或错误；普通 `user` 角色 JWT 访问 |

### 用户面：我的临时额度

`GET /user/temp-balances`（JWT 鉴权）

**强制只返回当前 JWT 用户本人的额度**——无 `user_id` 参数（对齐 `/user/stats` 模式防越权）。仅**有效额度**：`amount > 0` 且未过期（`expires_at` 为 `null` 的永久额度恒有效）；过期 / 用尽（0 元）/ 负扣减行一律隐藏。排序 `expires_at` 升序（FEFO 同序）、永久额度最后。

响应 `200`：

```json
{
  "total_usd": 6.5,
  "rows": [
    { "id": 2, "amount_usd": 1.5, "expires_at": "2026-09-14T10:00:00Z", "note": "redemption code" },
    { "id": 1, "amount_usd": 5, "expires_at": null, "note": "signup bonus" }
  ]
}
```

`total_usd` 为有效额度合计（**毫分 Σ 后一次性 /1e5**——避免逐行浮点累加误差；无有效额度 = `0`）。

| 状态码 | 场景 |
|---|---|
| `200` | 有效临时额度列表（`rows` 空数组 = 无有效额度） |
| `401` | 无 / 非法 JWT |

---

## 日志与统计

### 查询用量日志

`GET /api/admin/usage_logs?limit=20&cursor=1234&group_id=1&account_id=2&model=gpt-4o&error_type=none&from=2026-08-06T00:00:00Z&to=2026-08-06T23:59:59Z`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数（上限 200，超限裁剪到 200） |
| `cursor` | int | — | keyset 游标（上一页 `next_cursor` = 上页最后一条 id；缺失或 ≤0 = 首页） |
| `group_id` | int | — | 按分组过滤 |
| `account_id` | int | — | 按账号过滤 |
| `model` | string | — | 按模型名过滤 |
| `error_type` | string | — | 按错误类型过滤（值域收敛 `none` / `abort`——usage_logs 仅放行路径行） |
| `from` / `to` | RFC3339 | 必填 | 时间范围过滤（缺失 → 400；防无范围全分区扫描） |

> **分表设计**（用户裁决）：`usage_logs` 只承载**放行路径明细**——成功（`error_type=none`，含免费分组/0 token 成功行，cost 不限）+ abort 半异常计费行；4xx/5xx/拒绝等失败行**不入 usage_logs**（错误审计面见 `/err_logs`，下节）。

响应 `200`：

```json
{
  "rows": [
    {
      "ID": 1,
      "RequestID": "uuid",
      "GroupID": 1,
      "AccountID": 2,
      "TemplateID": 1,
      "Model": "gpt-4o",
      "MappedModel": "gpt-4o-2024-11-20",
      "Format": "openai-chat",
      "ErrorType": "none",
      "LatencyMS": 125,
      "InputTokens": 10,
      "OutputTokens": 20,
      "TotalTokens": 30,
      "Cost": 500,
      "BillingTier": "auto",
      "AboveHit": false,
      "Overdraft": false,
      "CreatedAt": "2026-08-06T10:00:00Z"
    }
  ],
  "next_cursor": 1234
}
```

> **游标分页语义**：行按 id 严格降序（id 全局单调，跨分区天然有序）；`next_cursor` = 本页最后一条 id，非 null 表示还有下一页（服务端 limit+1 探测多取 1 行）；下一页请求把 `next_cursor` 原样作为 `cursor` 参数（`WHERE id < cursor`），翻至 `next_cursor` 为 null（末页）。`total` 已从契约移除（游标语义下无全量计数）。

**计费字段**：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Cost` | int64 | 计费成本（**毫分**，1 USD = 100,000 毫分）；放行路径行可为 0（免费分组/0 token） |
| `RawCost` | int64 | 原始成本（**毫分**，乘倍率前——免费组 `Cost`=0 但 `RawCost` 有值，"实际消耗"口径）；bill 未装配/无价防御路径恒 0 |
| `BillingTier` | string | 请求 `service_tier` 归一化值：`priority` / `flex` / `fast` / `auto`（未知/空值归一 auto）；空 = 未计费路径（billing 关闭或未鉴权） |
| `AboveHit` | bool | 任一分量超 `above_threshold` 命中分段计价 |
| `Overdraft` | bool | 本次扣费透支（余额不足扣为负余额；`[billing]` 开启且允许透支时可能为 true） |

### 查询错误日志

`GET /api/admin/err_logs?limit=20&cursor=1234&group_id=1&account_id=2&model=gpt-4o&status_code=429&error_type=billing&from=2026-08-06T00:00:00Z&to=2026-08-06T23:59:59Z`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数（上限 200，超限裁剪到 200） |
| `cursor` | int | — | keyset 游标（上一页 `next_cursor` = 上页最后一条 id；缺失或 ≤0 = 首页） |
| `group_id` / `account_id` / `user_id` | int | — | 按归属过滤（`user_id`：admin 看全部） |
| `model` | string | — | 按模型名过滤 |
| `status_code` | int | — | 按状态码过滤（401/429/402/4xx/5xx…全值） |
| `error_type` | string | — | 按错误类型过滤（`auth`/`429`/`billing`/`4xx`/`5xx`/`network`/`abort`…） |
| `from` / `to` | RFC3339 | 必填 | 时间范围过滤（缺失 → 400；防无范围全分区扫描） |

> **错误明细面**：`err_logs` 承载**全部错误行**——本地拒绝（401 鉴权失败/429 限流/402 余额/tier 拒绝/缺价/no_account）+ 上游失败（4xx 透传/5xx 耗尽/network）+ abort 双轨行（与 usage_logs 关联，`request_id` 关联）。错误文本落 `error_message`（域内截断 500）。

响应 `200` 行结构与上节同构（`rows` + `next_cursor`，游标语义相同——`next_cursor` 非 null 表示还有下一页），但含完整错误面字段：`StatusCode`、`ErrorType`、`ErrorMessage`、`BillingTier`（tier 拒绝审计）。

> **存储（三表分区 + 独立保留期）**：`usage_logs` / `err_logs` / `usage_stats` 均为 PostgreSQL **按日分区表**（`PARTITION BY RANGE`，分区名 `{表名}_YYYYMMDD`；usage_logs/err_logs 分区键 `created_at`，usage_stats 分区键 `bucket_time`——小时桶聚合 24 桶/日分区）。保留期独立：`usage.log_retention_days`（默认 30 天）/ `usage.errlog_retention_days`（默认 7 天短保留——错误审计）/ `usage.stats_retention_days`（默认 180 天——聚合统计长保留）；retention worker 每小时 `DROP` 过期分区（O(1)，PG DELETE 不释放空间——清理必须分区 DROP）并预建未来分区。跨分区查询按时间范围走分区剪枝。

### 查询用量统计

统计面**已按查询形状拆成四个端点**（tag `stats`；路径写在 `paths:` 里不带 `/admin`，但挂 `/api/admin` 组，故实际为 `/api/admin/stats/*`）。用户面另有 `user` tag 的两个等价端点，强制 `user_id` = 当前用户（防越权）。

| 端点 | 形状 | 响应 |
|---|---|---|
| `GET /api/admin/stats/trend` | 时间桶趋势（cube 或原始行——按 `timezone` 路由） | `StatTrendPoint[]` |
| `GET /api/admin/stats/entity-trend` | 单实体时间桶趋势（强制 `entity` + `id`） | `StatTrendPoint[]` |
| `GET /api/admin/stats/top` | 实体排行（无时间桶，恒走 cube 绝对区间） | `StatTopEntry[]` |
| `GET /api/admin/stats/ttft` | TTFT 聚合（`entity` 空 = 平台级 sketch 分支；非空 = 实体级 exact 分支） | `StatTTFTSummary` |
| `GET /api/admin/stats/capabilities` | 本部署能查多久（只读、无参数；per-deployment 常量） | `StatsCapabilities` |
| `GET /api/user/stats` | 我的趋势（= `/stats/trend`，强制 `user_id` = 当前用户） | `StatTrendPoint[]` |
| `GET /api/user/stats/ttft` | 我的 TTFT（= `/stats/ttft`，强制 `user_id` = 当前用户） | `StatTTFTSummary` |
| `GET /api/user/stats/capabilities` | 同一份能力投影（用户面变体；`/api/admin/*` 只接受 platform_admin 凭据） | `StatsCapabilities` |

**`/stats/trend`（与 `/api/user/stats` 同形）**

| 查询参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `from` / `to` | RFC3339 | 与 `window` 恰择一 | 绝对窗口（显式值为绝对时刻，不受时区改写）。与 `window` 同给、只给一端、或两者都不给 → `400` `reason=window_ambiguous`；`from` 不早于 `to` → `400` `reason=window_invalid` |
| `window` | string | 与 `from`/`to` 恰择一 | 相对窗口，**只收** Go `time.ParseDuration` 时长串（`24h` / `168h` / `2160h`）。服务端自持时钟：`to` = 当前时刻向上取整到 UTC 整点、`from` = `to` − `window`，故两端恒整点、零对齐位移。`7d` 这类日历天不是时长单位 → `400` `reason=window_invalid`（`24h` 在 DST 切换日 ≠ 一个日历天，不另立一套长度语义） |
| `granularity` | `hour` / `day` | 否（缺省 `day`） | 桶粒度 |
| `group_id` | int64 | 否 | 维度过滤（仅管理面有） |
| `model` | string | 否 | 模型过滤 |
| `timezone` | string | 否 | IANA 名（通常 = 浏览器时区；空 = UTC；非法 → `400`）决定桶界所在时区——显式 `from`/`to` 恒为绝对时刻，不受时区影响。它不再推出任何缺省窗口 |

**`timezone` 是路由键，不只是展示**：

- 时区偏移恒整点、窗内无 DST 跳变、且窗口界对齐 → 读卷积表（`usage_stats`/`usage_entity_stats`，预聚合）。界不齐时服务端把两端**向后取整到整点**换取卷积表（窗口微移 <1h，不丢服务可查的起点），生效窗口用响应头 `X-Stats-Effective-From` / `X-Stats-Effective-To` 回显。
- 无法走卷积表（`:30`/`:45` 偏移，或窗内跨 DST 跳变）→ 只能扫 `usage_logs` + `err_logs` **原始明细**分组。
- **上限不是一个固定天数**：成本上限（cost）是**保留期无关的纯常量**——分组原始行 8 天、卷积表 90 天；覆盖面另有 per-table 保留期闸门（起点早于该表保留截止 → `400`）。两者互相独立，且**真实数值随部署而变**（`usage.log_retention_days` / `usage.errlog_retention_days` / `usage.stats_retention_days`）——`GET /api/admin/stats/capabilities` 报的就是本部署的这两个量（`cost_cap_seconds` 与 `coverage_days`，`0` = 无上限）。超限一律 `400`（宁可报错，不静默残缺）。

**`/stats/entity-trend`**：窗口与 `trend` 同形（`from`+`to` 或 `window` 恰择一）。额外必填 `entity`（`account` / `user` / `key`）+ `id`（实体 id），`granularity` 必填，可选 `model`；时区路由同 `trend`。

**`/stats/top`**：必填 `from` / `to` / `entity`（`account` / `user` / `key`）/ `by`（`cost` / `requests` / `tokens`），可选 `limit`（缺省 20，`1`–`200`，超限裁剪不报错）。**不接受 `window`**——排行无预设、无对齐分支，`from` / `to` 保持必填。排行按实体聚合、**无时间桶**，数值与时区无关——`timezone` 仅接受并校验（客户端统一带浏览器时区，非法名照旧 `400`），不进查询。窗口仍过同一套判定（cost + 覆盖面），**本部署的实际上限见 `/stats/capabilities` 的 `top`**。

**`/stats/ttft`（与 `/api/user/stats/ttft` 同形）**：窗口与 `trend` 同形（`from`+`to` 或 `window` 恰择一），可选 `entity`（`account` / `user` / `key`）+ `id`（**成对使用**），可选 `model`。分位数与计数是绝对区间数值——`timezone` 仅接受并校验（非法 `400`），不进查询、不进缓存键。两个分支的窗口上限不同，**取值见 `/stats/capabilities` 的 `ttft_sketch` / `ttft_exact`**（不改常量的前提下：`entity` 空 = 平台级 sketch 分支（cube 直方图服务端合并）90 天；`entity` 非空 = 实体级 exact 分支（打 `usage_logs` 原始行 `percentile_cont`）168h）。

**`/stats/capabilities`（`/api/user/stats/capabilities` 为用户面同一投影）**：只读、无参数、可按部署缓存。返回每个读形状（`kinds` 键）的候选存储 `storages`、成本上限秒数 `cost_cap_seconds` 与覆盖天数 `coverage_days`：

- `cost_cap_seconds = 0` = 无上限（`usage_list`/`errlog_list` 是 keyset 分页，成本由分页与索引决定，与跨度无关）；`coverage_days = 0` = 该表未启用分区保留（覆盖闸门关闭）。
- 两个量**互相独立**：成本上限是保留期无关的纯常量（扫 10 天日志的成本与保留了多少天日志无关），覆盖天数取自该读路径实际要读的表（读多张表取最保守者——统计面 raw 读 `usage_logs` + `err_logs`，故为 `min(两表保留期)`；`errlog_list` 只读 `err_logs`，故就等于 `errlog_retention_days`）。
- 它只报**按部署恒定**的事实，**不报"这个时区能否用卷积表"**：后者是逐请求判定（依赖窗口位置），由下面的回显头与 400 机读字段承担。前端按它裁剪选择器与窗口上限（不再有客户端硬编码）。

**成功响应回显头**（`/stats/trend`、`/stats/entity-trend`、`/api/user/stats`——这三个端点 200 体是**裸数组**，加字段即破坏契约，故回显走头）：`X-Stats-Effective-From` / `X-Stats-Effective-To`（RFC3339，UTC，本次读取**真正使用**的半开窗口）/ `X-Stats-Storage`（`cube` / `raw`）。`/overview` **没有**回显头——它的窗口由服务端按 `days` 自算、恒为本地日界（整点），从不发生位移。

**拒绝时的机读字段**（`400` 响应体，全部**可选**，故既有客户端零影响）：`reason`、`storage`、`limit_seconds`、`effective_from`、`effective_to`、`retention_days`。`reason` 取值：`window_invalid`（必填/倒序/`window` 时长串不可解析——此时**只有** `reason`，没有可报的存储与上限）、`window_ambiguous`（`from`/`to`/`window` 未恰择一：两态都给、只给一端、或纳入端点上两者都不给——同样**只有** `reason`）、`window_too_long`（请求跨度超过该形状的成本上限，携 `limit_seconds`）、`raw_horizon` / `cube_horizon`（起点早于所要读表的保留截止，携 `retention_days`）。`error` 的人读文案形态不变。

`StatTrendPoint` 字段：`BucketTime` / `RequestCount` / `ErrorCount` / `CallCount` / `InputTokens` / `OutputTokens` / `TotalTokens` / `CacheReadTokens` / `CacheCreationTokens` / `Cost` / `RawCost` / `TTFTAvgMS` / `TTFTMaxMS`。

- `Cost` / `RawCost`（float64 **USD**）= 内部毫分 /1e5，与价格 API、`/api/admin/overview` 口径一致（破坏性变更：旧版为毫分 int64）。`RawCost` 是乘倍率前的"实际消耗"口径（免费组 `Cost` 为 0 但 `RawCost` 有值）。
- `CallCount` = 按次调用计数（图片生成张数 / search 次数；**不入** `TotalTokens`）。
- `TTFTAvgMS` = `TTFTTotalMS / TTFTCount`（无样本 0）；`TTFTMaxMS` 最大值。

`StatTopEntry` 字段：`EntityType` / `EntityID` + 与 trend 同名的计数与 `Cost` / `RawCost`（USD）+ `TTFTAvgMS` / `TTFTMaxMS`。

`StatTTFTSummary` 字段：`Count` / `AvgMS` / `P50MS` / `P95MS` / `P99MS` / `MaxMS` / `Source`（`exact` = 实体级原始行分位；`sketch` = 平台级 cube 直方图合并）。

> `TTFT*` 统计**仅含首 token 流式请求**（非流式 / 失败 / 无首 token 行不计）。前端跨行合并语义：avg 加权（`Σ(avg×count)/Σcount`）、max 取最大、**pN 取请求量最大维度行的近似值**（分位数不可跨行合并）。

> 统计由**离线聚合 worker** 每 5 分钟从 `usage_logs` / `err_logs` 重算落盘（watermark + 覆盖语义），查询结果可能有 ≤5 分钟延迟；错误桶 = abort 行（usage_logs 全字段）+ 纯错误行（err_logs count 语义，tokens/cost/TTFT 恒 0）；拒绝行（限流）随 err_logs 采样——风暴时错误计数可能低估。

---

## 管理端总览

### 总览聚合

`GET /api/admin/overview?days=7&group_id=1`——dashboard 主数据一站式聚合

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `days` | int | 7 | 趋势天数（含今日；上限 30） |
| `group_id` | int | — | 按组过滤聚合（summary/trend；缺省全局） |

响应 `200`（`OverviewResponse`）：

```json
{
  "summary": {
    "requests": 12345, "errors": 234, "err_rate": 0.019,
    "cost_usd": 1.23456, "call_count": 12,
    "input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
    "cache_read_tokens": 0, "cache_creation_tokens": 0,
    "ttft_avg_ms": 620.5, "ttft_max_ms": 3800,
    "ttft_p50_ms": 500, "ttft_p90_ms": 1500, "ttft_p95_ms": 2100, "ttft_p99_ms": 3400
  },
  "trend": [
    { "date": "2026-08-14", "requests": 2345, "errors": 45, "cost_usd": 0.23456,
      "tokens": 99999, "call_count": 3,
      "ttft_avg_ms": 610.2, "ttft_max_ms": 3500,
      "ttft_p50_ms": 490, "ttft_p90_ms": 1400, "ttft_p95_ms": 2000, "ttft_p99_ms": 3200 }
  ],
  "accounts": { "active": 12, "unhealthy": 1, "429": 0, "disabled": 2,
                "concurrency": 3, "max_concurrency": 40 },
  "resources": { "templates": 8, "groups": 4, "users": 15 },
  "err_top": [ { "name": "account-01", "err_rate": 0.05, "err_count": 12 } ],
  "alerts": { "billing_lag_ms": 12345, "billing_unbilled_rows": 123, "billing_quarantined_rows": 0 }
}
```

- `summary`：今日汇总（请求 `timezone` 时区日界，缺省 = UTC），`cost_usd` 为 USD（毫分 /1e5），`ttft_*` 口径同 `/api/admin/stats/ttft`
- `trend`：近 N 天日桶（SQL 侧按请求时区日界聚合；`tokens` = input+output+cache 合并；DST/半小时偏移时区扫原始明细，超上限 = 400——上限见 `/api/admin/stats/capabilities` 的 `summary`/`days`）
- `accounts`：账号健康分布 + 并发水位（**调度器快照同源**——与账号列表运行时视图一致；运行时状态只在内存，DB 无第二份）
- `err_top`：账号维度错误率 Top5（调度器 EWMA，`name` = 账号名）
- `alerts`：billing 游标积压观测（lag 族——`billing_lag_ms` 时滞毫秒 / `billing_unbilled_rows` 未扣费行数 / `billing_quarantined_rows` 隔离行数累计）
- 聚合结果内部 TTL 30s 缓存（键含 `days`/`group_id`、请求 `timezone` 与该时区日界）；统计本身为离线聚合产物（≤5 分钟陈旧）

### 实时并发排行

`GET /api/admin/users-top?top=20`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `top` | int | 20 | TopN（上限 100） |

响应 `200`：

```json
{
  "users": [ { "user_id": 3, "email": "alice@x.com", "concurrency": 12 } ],
  "other_concurrency": 45
}
```

- 实时在途并发降序 TopN + `other_concurrency`（其余在途用户合计）
- 数据源 = 门禁并发快照（`Auth.InFlightUsers` 只读访问器，零锁）；内部 TTL 2s
- **本实例视角**：多实例部署下各实例独立计数，展示为当前实例视图

### 运维观测

`GET /ops/workers`——worker 运行状态（billing / invalidate / notify / pricing / retention / usage / **stats-agg** / **quality-sync** 等，各 worker `Stats()` 原样输出；路由计划编译器不是独立 worker——它是 scheduler 内的串行编译道，成功/失败新鲜度经 scheduler Stats 上报）。`stats-agg`（离线聚合 worker）四字段：

| 字段 | 说明 |
|---|---|
| `watermark_unix_ms` | 聚合水位（毫秒；0 = 未初始化/首轮未完成） |
| `last_buckets` | 上轮写入桶数（失败轮保留上轮值） |
| `last_rows` | 上轮消费明细行数（三查询合计） |
| `last_duration_ms` | 上轮耗时（毫秒） |

`retention`（分区保留 worker）除巡检时刻与逐表 DROP 计数外，另有一组**保留期兜底观测**——路由事实表的有界性完全依赖该 worker 在跑，worker 停摆或 DROP 持续失败时分区会**静默无界增长**，这组字段是该失效的唯一观测面。聚合值是告警，分表值是诊断（一表 DROP 失败而另一表健康时，聚合只显示漂移，分表值才指出故障表）：

| 字段 | 说明 |
|---|---|
| `oldest_partition_unix_ms` | `routing_quality_fact` / `routing_flow_fact` 两张表的最老分区下界（毫秒；0 = 无分区）。**早于观测保留 cutoff（`routing.observation_retention_days`）即 Warn**——正常巡检下不可能出现 |
| `partition_count` | 上述两张表的分区总数 |
| `quality_partition_count` / `quality_oldest_partition_unix_ms` | `routing_quality_fact` 单表分区数 / 最老分区下界（毫秒；0 = 该表无可解析分区） |
| `flow_partition_count` / `flow_oldest_partition_unix_ms` | `routing_flow_fact` 单表分区数 / 最老分区下界（口径同上） |
| `last_dropped_routing_quality_partitions` / `last_dropped_routing_flow_partitions` | 最近成功轮两张事实表各自 DROP 分区数（失败轮保留上轮值；0 也可能 = 无过期分区，是否失败看 Warn 日志） |
| `snapshot_state_rows` | `routing_flow_snapshot_state` 当前总行数（普通表，无分区可 DROP，DELETE 失败即无界增长） |
| `undated_partition_count` | 名解析失败的分区数（既不计数也不 DROP，只能人工介入） |
| `partition_stats_stale` | `true` = 分区统计查询失败，当前呈现的是上轮过期值（`last_patrol_unix_ms` 仍推进） |
| `routing_observation_retention_days` | 路由观测保留天数（`oldest_partition_unix_ms` 的 cutoff 解释口径：`cutoff = now - 本值`） |

---

## 规则 Rules

规则引擎是路由健康政策的唯一升级通道：上游 attempt 结果事件按 `priority` 升序逐规则首中匹配，命中即执行 `then` typed 动作（`throttle` 瞬时限流 / `fail_account` 终态判死 / 响应塑形）。规则变更（增删改）即时生效（自动触发引擎重载 + NOTIFY 全实例同步）。

### 规则模型

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 规则名（唯一） |
| `priority` | int | 优先级（唯一，升序匹配、首中即停） |
| `enabled` | bool | 缺省 `true`；`false` = 停用（不参与匹配） |
| `when` | object | 匹配条件（字段白名单，未知字段拒绝 400） |
| `then` | object | 动作集（`throttle` / `fail_account` / `response_code` / `custom_message`，见下） |

`when` 字段（全部可选，nil = 不限）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `kind` | string | `ok` / `429` / `4xx` / `5xx` / `network`（nil = 任意事件） |
| `http_status` | int | 上游状态码等值匹配 |
| `error_message_contains` | string | 错误消息子串匹配 |
| `account_id` / `template_id` / `group_id` | int | 维度等值匹配（nil = 不限） |
| `model` | string | 模型等值匹配（最终请求模型，映射后 sel.Model / MappedModel；无映射时退化为请求模型；空串非法，validate 拒绝） |
| `window_seconds` | int | 统计窗口（≥1，缺省 60；固定粒度近似，误差 ≤ 一个粒度） |
| `count_429_ge` / `count_failure_ge` / `count_ok_ge` / `count_total_ge` | int | 窗口内计数阈值（≥0）；`count_failure_ge` 语义 = 失败事件桶（4xx/5xx/network 并入） |
| `ratio_429_ge` / `ratio_failure_ge` | float | 窗口内比例阈值（[0,1]，**必须配 `count_total_ge`**）；`ratio_failure_ge` 分母为失败事件桶 |

`then` 动作（指针即意图：键缺省/null = 该维度透传不改；全空 `{}` = 纯透传规则）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `throttle` | object | typed 瞬时限流：`{scope: account\|account_route, mode: retry_after\|open, duration_ms?, use_reset}`。`scope=account` 作用该账号全部 RouteClass；`account_route` 只作用命中的单 RouteClass（并**有意传播**到同账号映射同质量类的其它 RouteClass）。`mode=retry_after` 要求 `use_reset=true`（上游 Reset 头优先，`duration_ms` 可选回退）；`mode=open` 要求 `use_reset=false` 且 `duration_ms > 0`（固定摘除窗口）。到期不自动 READY——进入 PROBING 由单 permit 探针接管 |
| `fail_account` | bool | `true` = 命中即判死（终态失效：`failed_at` + `failure_source=rule`，CAS fenced；恢复唯一入口 `POST /accounts/{id}/recover`） |
| `response_code` | int | 缺省/null = 透传上游状态码；设置 = 覆写为指定码（400-599） |
| `custom_message` | string | 缺省/null = 透传上游错误文案；设置 = 覆写为固定文案（禁止空串） |

`throttle` 与 `fail_account` 互斥（一条规则只出一个健康动作）。原始 429/5xx 事件本身**不直接**改变健康——只有命中规则的 `then` 才产生 `throttle`/`fail_account`（种子规则是无窗口条件的单 kind 匹配，单事件即命中；自定义规则可叠加窗口阈值）；质量统计即时吸收每次 attempt 结果，两者互不重复记账。

种子规则（规则表为空时引擎重载自动写入，共 5 条）：`kind=429` → retry_after 瞬时节流（上游 Reset 头优先，缺失回落 30s）+ 文案 "rate limited"（码透传 429，priority 10）、`kind=4xx + http_status=400` → 全透传（priority 15）、`kind=5xx + http_status=503 + error_message_contains="overload"` → 全透传不惩罚（priority 16）、`kind=5xx` → 10m 摘除 + 502/"Upstream request failed"（priority 20）、`kind=network` → 5s 摘除 + 502/"Upstream request failed"（连接级独立冷却，priority 25）。**无 ok 恢复种子**：健康恢复的唯一自动路径是 RuntimeHealth 探针——OPEN/RETRY_AFTER 窗口内不探测（跑满 TTL，到期按 Sync 保留规则回归：多数直接回 READY 交还真实流量，保留转换才进 PROBING）；探针只服务 PROBING 条目（recover 生命周期 SetProbing + 保留转换），单 permit、两次成功 READY。api_key 族无合成探测面（/v1/models 与流量无关，已删除——PROBING 视为通过，恢复由时间窗+真实流量判定），codex 族走 SDK usage 快照。成功事件不驱动状态机。删除全部规则后，下次引擎重载（任意规则 CRUD 或重启）会自动重新播种；播种仅看规则表**为空**——全部禁用（行仍在）不触发播种，引擎以零规则运行。

### 事件模型与匹配语义

- **事件来源**：每次上游 dispatch attempt 终态产生一个事件 `{kind, http_status, error_message, account_id, template_id, group_id, model, occurred_at, reset_at, candidate_fingerprint}`，经有界队列投递规则 worker 匹配（队列满时丢弃并计数告警，不阻塞请求路径——**整链 best-effort 是有意契约**：风暴下允许丢事件，不丢请求）。
- **条件投递**：仅当规则集中存在 `when.kind` 为 `nil`（任意）或 `ok` 的规则时，`ok` 事件才进入匹配；否则 ok 事件直接被跳过（性能优化）。ok 事件只服务于自定义观测/恢复类规则（种子规则不含 ok 规则——健康恢复由 RuntimeHealth 探针承担，见上节）。
- **首中即停**：按 `priority` 升序逐规则匹配，首个命中即执行其 `then` 全部动作，不再继续。
- **命中不清零窗口**：计数窗口为滑动窗口，命中不重置计数（自然衰减）；统计窗口固定粒度近似，误差 ≤ 一个粒度。
- **throttle 语义**：命中且 `then.throttle` 提供 → RuntimeHealth 状态机迁移（`READY → RETRY_AFTER|OPEN → PROBING → READY`），键 `(account, QualityClassID|wildcard, identity_revision)`；跨实例 ≤1s 收敛（Redis 全量快照 + 定向事件）。到期不自动 READY——进入 PROBING 由单 permit 探针接管。
- **fail_account 语义**：命中即走统一 FailureHandler：进程本地 fail-closed latch 先生效（本实例立即禁选，同代际静态重载不清除），PG CAS 成功才落 `failed_at` + C +1（K 不变——失效写入不是身份写入）+ 组级 NOTIFY（其它实例收敛；PG 写失败不重试——latch 兜底本实例，远端由其它路径收敛）。
- **动作生效即时性**：规则增删改自动触发引擎重载（全实例同步，重载清窗口计数）；命中动作本地即时应用，持久化/跨实例同步走有界队列尽力投递（可丢，无 outbox——四类丢弃指标 ops 可见）。

### 创建规则

`POST /api/admin/rules`

```json
{
  "name": "escalate-on-5xx",
  "priority": 40,
  "enabled": true,
  "when": { "kind": "5xx", "count_failure_ge": 5, "window_seconds": 60 },
  "then": { "throttle": { "scope": "account", "mode": "open", "duration_ms": 30000, "use_reset": false } }
}
```

响应 `201`：创建后的规则（含 `id`/`created_at`/`updated_at`，`when`/`then` 原样返回）。


> **复合多值 IN（fresh setup）**：
> - `when.http_status_in` / `when.model_in` / `when.error_message_contains_in` 为可选数组，与同名单值字段**互斥**（同时提供 → `400`）
> - 同字段**OR**语义（`model_in: [a,b]` 任一命中即过），跨字段**AND**，按 `priority` 首条命中
> - `model`/`model_in` 匹配**最终请求模型**（sel.Model/mapped），响应/状态/日志三面一致
> - 元素校验：`http_status_in` 400-599且去重，`model_in`/`error_message_contains_in` 非空去重；`kind=ok` 拒 `error_message_contains_in` 
> - Upgrade: `DisallowUnknownFields` 下旧部署发 `_in` 判 `400`，新库 **fresh setup 空库重建**（无迁移）
> - 性能：复合 IN 预编译集合，Classify 零分配（与 strip_scan 热路径纪律对齐）

### 规则列表

`GET /api/admin/rules?enabled=true`（`enabled` 可选，缺省返回全部；priority 升序，无分页）

响应 `200`：`{"total": N, "rows": [...]}`。

### 更新规则

`PUT /api/admin/rules/{id}`——部分更新：未提供的字段保持原值（`when`/`then` 提供即整体替换）。

响应 `200`：更新后的规则；`404` 含缺失 id。

### 删除规则

`DELETE /api/admin/rules/{id}`——响应 `204`；`404` 含缺失 id。

`POST /api/admin/rules/batch-delete`——请求体 `{"ids": [...]}`（1-100 条，自动去重）；事务全成或全败；响应 `200` `{"deleted": N}`；`404` 消息含缺失 id。注意：批量删除后触发规则引擎重载，若规则表删空则下次重载自动重建种子规则。

### 错误语义

| 状态码 | 场景 |
|---|---|
| `400` | `when`/`then` 含未知字段、`kind` 非法、计数为负、`window_seconds` < 1、比例越界或缺 `count_total_ge`、`then` 无动作、`throttle` 组合非法（scope/mode/duration/use_reset 真值表不满足、与 `fail_account` 并现）、`custom_message` 空串 |
| `409` | `priority` 或 `name` 唯一冲突 |

> 配置变更：`scheduler.cooldown_429` / `scheduler.backoff_base` / `scheduler.backoff_max` **已移除**（2026-08-13 用户裁决：不向后兼容）——配置含这些键将启动失败（未知键报错）。429 节流、错误摘除与恢复节奏统一由规则引擎的种子规则与自定义 typed 动作（`throttle`/`fail_account`）接管。

---

## 路由观测 Routing

智能路由数据链：热路径每次 attempt 终态进 quality recorder（内存归并），`quality-sync` worker 串行 loop 定期把质量行**逐行绝对量 UPSERT 进质量事实表**（`routing_quality_fact`——唯一质量存储，无实例分钟表、无重算车道、无脏位/水位），flow 快照则**直写**（`routing_flow_fact`，按 `instance_src` 分片、整分片替换——无下游重算）。观测保留深度由 `[routing] observation_retention_days` 决定（默认 7 天，独立于 `usage.stats_retention_days`）；同一保留巡检对两张事实表按 cutoff DROP 日分区，并清理 `routing_flow_snapshot_state`。选号面为 **plan-only**：无计划外车道，AI 派生身份一律由编译计划背书；冷启动窗口（编译决策视图未发布）与编译滞后窗口（静态已装载、该路由桶尚未编出 → `ErrPlanNotReady`）AI 流量一律 `503` + `Retry-After: 1`（管理面/用户面/healthz 不经此门，空库照常发布空决策）；仅真正不可路由（模型未映射/格式无候选/组不存在）fail-closed `404`（不再以占位身份转发）。

三只读观测端点（数据源钉死两张事实表 routing_quality_fact / routing_flow_fact 与当前发布 RoutingView——不查 raw logs、不按历史 generation 查询）：

| 方法/路径 | 说明 |
|---|---|
| `GET /api/admin/routing/plan` | 当前发布路由计划解释：generation + 全路由 primary/explore/degraded 候选发布序 + explore 权重/累积表 + 候选静态身份。空视图 = generation 0 空计划（`routes: []`），不是错误。稳态探索份额 ExploreBP（无 Primary=10000bp；否则 100+min(400, ⌈400×unknown/eligible⌉)bp，上限 500bp）编入计划，请求侧按 canonical 哈希 + 该份额每请求定车道。每路由附带事故状态 `incident`（`active/kind{domain|model|both}/comparable/degraded/domains/evaluated_minute`，detect+surface——不改车道、不节流、不探针；ops `scheduler` 卡 `active_incidents` 计数）。**查询参数**：`search`（标签模糊匹配，大小写不敏感；匹配 `model` / `format` / `operation_tag` / `group_id` 十进制串）、`route`（64-hex；给定则**优先于 `search`**，只返回该路由；非法 hex → `400`，未知 → `404`）、`offset`/`limit`（路由分页，缺省 0/20，上限 200）、`candidates_offset`/`candidates_limit`（**每个返回路由各自**的候选分页，缺省 0/20，上限 200；**显式 `0` = 不返回候选**）。**响应新增** `total_routes`（`search` 过滤后总数）与每路由 `candidates_total`（切片前总数） |
| `GET /api/admin/routing/frontier?route=<64hex>&from&to&offset&limit` | 质量-成本前沿（窗口 rollup × 当前计划候选连接）：Wilson95 成功区间 + TTFT 区间 + 每次成功平均成本，Pareto 前沿标记；窗口 ≤90 天，`offset`/`limit` 缺省 0/20、上限 200（超出钳制）。**排序与前沿标记在切片之前**，故分页不改变前沿判定；响应含 `total_candidates`（完整候选数，不受分页影响）。TTFT 区间仅由流式首 token 样本贡献（非流式/失败不计入 `ttft_n`）。**`cost_per_success` 的单位是毫分 int64**（1 USD = 100,000 毫分，与 compiler 同式；属明细/计数面，API 边界不做 USD 换算，`cost_known=false` 时无意义） |
| `GET /api/admin/routing/flow?route=<64hex>&from&to&offset&limit&accounts` | 路由 flow 聚合（Sankey 数据）：RouteClass → (ordinal, lane) → Account → Outcome 完整链边，按 terminal_at 归属；窗口 ≤90 天。`offset`/`limit`（缺省 0/20，上限 200）**只作用于边表分页**；`accounts`（缺省 20，上限 200）**只作用于桑基每层保留账号数**（其余折叠为 `folded=true` 的占位节点）。**`lanes` 只是完整边集的一页**；守恒计数、`total_edges`、`stale_generation_present` 与 `sankey` 恒在**完整边集**上算，与当前页无关。响应：`total_edges`（完整边**行**数，驱动前端翻页器）、`stale_generation_present`（布尔：完整边集内是否存在 `min_generation != plan_generation` 的边行）、`sankey`（服务端折叠后的 `RoutingFlowGraph`：`edges`/`account_limit`/`folded_accounts`/`folded`）。`folded_accounts` 是**各 `(ordinal,lane)` 层被折叠账号数之和**（同账号两层都折叠则计两次），故**不是**去重账号数 |

**缓存亲和（请求级软亲和，非硬钉位）**：请求携带 `prompt_cache_key` / `conversation_id` / `session_id`（按此优先级取首个非空字符串；REST 面单遍提体扫描、responses-ws 从首帧提取，search 不参与）时，键值经 FNV-1a 哈希在一致性哈希环（每域 32 虚拟节点）上定位属主缓存域，计划内属主域候选整体前置、其余候选按原相对顺序顺延——候选集合与 1–8 次尝试上界不变。账号 `cache_domain` 相同 = 共享域（互相亲和命中），`null` = 账号私有域（仅自身可被亲和命中）。无亲和键 = 严格按计划编译原序执行。跨轮次硬续聊钉位（continuation pinning）见对应 continuation 接口与行为约束。

flow 守恒与丢失口径（三者独立，不得混为上游失败）：`incomplete_chain_dropped` = 本进程已观察 cleanup 缺 terminal；`flow_overflow_dropped_chains` = 故障预算淘汰链；`process_crash_loss_unobservable` 恒 `true`（硬崩缺口不可量化）。

旧代际徽标口径：服务端只输出**精确布尔** `stale_generation_present`（完整边集内存在 `min_generation != plan_generation` 的边行），前端据此显示/隐藏徽标，**不再计算占比**——`stale_chains` / `total_chains` 两字段已退役。退役理由：合并层把多代际的链折叠进一行（`chain_count` 求和、`min_generation` 取最小），链次加权谓词只能给出**上界**（一行只要含任一旧代际链就整行计入；例：一行折叠 `{gen5:10, gen6:7, gen7:5}` 存为 `chain_count=22`、`min_generation=5`，`plan_generation=7` 时 22 全算陈旧而实际只有 17），而精确的逐代际链数不可存（"当前代际"是移动靶）。布尔是**行级谓词**，无归属误差：`min_generation` 恰为该行链的最老代际，故谓词为真 ⟺ 窗口内存在非当前计划代际的链。**精确性前提**：`generation` 随发布单调不减，故存量行恒有 `min_generation ≤ plan_generation`；未来代际竞态（行内含更新代际但 `min_generation == plan_generation`）不在目标场景内。布尔基于**完整边集**，翻页不变。

---

## 兑换码 Redemption Codes

兑换码是资源发放的通用载体（计费前基础设施）：生成一批码 → 分发给用户 → 用户在 `/user/redemptions` 兑换 → 资源按码类型即时生效。管理面 5 个端点 + 用户面 2 个端点。

### 生成兑换码

`POST /api/admin/redemption-codes`

请求体：

```json
{
  "type": "balance",
  "value": 100,
  "remark": "618 活动",
  "expires_at": "2026-12-31T23:59:59+08:00",
  "resource_expires_at": "2027-01-15T00:00:00+08:00",
  "max_uses": 1,
  "count": 5
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | string | ✅ | 枚举：`balance` / `concurrency` / `temp_balance`；非法 → `400` |
| `value` | double | ✅ | 面值：`balance`/`temp_balance` = **USD**（如 `10` = $10；`concurrency` = 并发数，必须整数否则 `400`）；`> 0`，否则 `400`（存储毫分，API 边界换算） |
| `remark` | string | 否 | 备注 |
| `expires_at` | datetime | 否 | 码**未兑换即过期**；缺省 = 永久；必须晚于当前时间（过去时间 → `400`） |
| `resource_expires_at` | datetime | 否 | 兑换后**资源到期**；`temp_balance` 必填且必须晚于当前时间，其余类型恒为 `null` |
| `max_uses` | int | 否 | 可兑换次数：`1` = 单次码（缺省）；`>1` = 多人码；`< 0` → `400` |
| `count` | int | 否 | 一次生成个数 `1–1000`（缺省 `1`）；`0` 或缺省 = 1；越界 → `400` |

响应 `200`：生成的完整码列表（`count` 个，码格式 `XXXX-XXXX-XXXX-XXXX`（16 字符，熵 ~80bit），字符集去易混淆的 `I/O/0/1`）。

```json
{
  "codes": [
    {
      "ID": 1,
      "Code": "JQVF2X-LD7SJQ",
      "Type": "balance",
      "Value": 100,
      "Remark": "618 活动",
      "ExpiresAt": "2026-12-31T23:59:59+08:00",
      "ResourceExpiresAt": null,
      "MaxUses": 1,
      "UsedCount": 0,
      "Status": "active",
      "CreatedBy": 0,
      "CreatedAt": "2026-08-08T10:00:00Z",
      "UpdatedAt": "2026-08-08T10:00:00Z"
    }
  ]
}
```

### 兑换码列表

`GET /api/admin/redemption-codes`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 页码，**1-based**；缺省或 `< 1` 按 1（不报错） |
| `page_size` | int | 20 | 每页行数；越界（`< 1` 或 `> 100`）→ `400` |
| `type` | string | — | 筛选枚举：`balance` / `concurrency` / `temp_balance`；非法 → `400` |
| `status` | string | — | 筛选枚举：`active` / `disabled`；非法 → `400` |
| `sort` | string | `id` | 白名单：`id` / `code` / `type` / `value` / `max_uses` / `used_count` / `status` / `created_by` / `created_at` / `updated_at`；非法 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |

响应 `200`：`{"total": N, "rows": [兑换码对象]}`（增强分页范式，与 templates 等旧 `limit`/`offset` 端点不同）。

### 批量失效

`POST /api/admin/redemption-codes/batch-deactivate`

请求体：`{"ids": [1, 2, 3]}`（`1–100` 条，去重；空或超 100 → `400`）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deactivated": N}`——N 为**新失效数**（已 `disabled` 的 id 为 no-op 不计入） |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（先查后失效，事务全成或全败） |

批量失效为单事务；重复提交（全部已失效）返回 `{"deactivated": 0}`（幂等重放友好）。

### 单码失效

`POST /api/admin/redemption-codes/{id}/deactivate`

无请求体。已失效再次调用为 no-op 成功（响应仍为 `{"deactivated": true}`，表示操作成功而非"本次新失效"）；`404`：id 不存在（消息含缺失 id）。

### 兑换记录（审计）

`GET /api/admin/redemption-codes/{id}/uses`

响应 `200`：

```json
{
  "total": 1,
  "rows": [
    { "ID": 1, "CodeID": 1, "UserID": 7, "Value": 100, "ResourceExpiresAt": null, "CreatedAt": "2026-08-08T10:05:00Z" }
  ]
}
```

`Value` 为兑换时的值快照（USD；`concurrency` 类型 = 并发数整数。码后续失效不影响历史记录）；`404`：码不存在。

### 用户面：兑换

`POST /user/redemptions`（JWT 鉴权，非 admin 面——见下方"鉴权与 created_by 约定"）

请求体：`{"code": "JQVF2X-LD7SJQ"}`。

响应 `200`：

```json
{
  "applied": {
    "type": "balance",
    "value": 100,
    "resource_expires_at": null
  }
}
```

`applied` 为实际生效的资源（事务内应用）：`balance` 加余额、`concurrency` 加并发数、`temp_balance` 加临时余额（`resource_expires_at` 非空）；`value` 单位与生成时一致（`balance`/`temp_balance` = USD）。任一步失败（含并发用尽/重复兑换）整体回滚，资源不变。

| 状态码 | 场景 |
|---|---|
| `200` | 兑换成功 |
| `400` | `invalid code`：码不存在 / 已失效 / 已过期 / 用尽——**统一不泄露具体原因**（防枚举探测） |
| `409` | `already redeemed`：本用户已兑换过该码（重复请求稳定 409，即使码随后失效也用尽，也与"已兑换"事实一致） |
| `401` | 无 / 非法 JWT |

### 用户面：我的兑换记录

`GET /user/redemptions`（JWT 鉴权）

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 1-based；缺省或 `< 1` 按 1 |
| `page_size` | int | 20 | 越界（`< 1` 或 `> 100`）→ `400` |
| `sort` | string | `id` | 白名单：`id` / `code_id` / `user_id` / `value` / `created_at`；非法 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |

响应 `200`：`{"total": N, "rows": [兑换记录]}`——记录含码的 `Code` / `CodeType` / `Remark` 联查快照。**强制只返回当前 JWT 用户本人的记录**（`user_id` 取自 JWT，无法通过参数指定他人——用户 A 永远看不到用户 B 的兑换记录）。

### 鉴权与 created_by 约定

| 路径 | 鉴权方式 | `created_by` 语义 |
|---|---|---|
| 静态 admin token（`Authorization: Bearer <admin.token>`） | `config.toml` 的 `admin.token` | 生成码时 `created_by = 0`（**0 = 系统**，未注入用户身份） |
| platform_admin JWT（`Authorization: Bearer <jwt>`） | 与 /user 面同签发的 JWT，且 `role == platform_admin` | 生成码时 `created_by = 该用户 id`（>`0`） |

`/api/admin/*` 两条路径任一通过即可；普通 `user` 角色的 JWT 访问 `/api/admin/*` → `401`。`created_by` 用于审计"哪个管理员/系统创建了这批发码"。

---

## 模型价格 Model Pricing

模型计费价格表（`price_entries` + `price_variants` 双表）：`price_entries` 每模型一行，`mode` 声明主计费方式（`token`/`call`/`image`），但价格分量跨模式可选配——token 行可携带 image/call 分量治 resp 图片旁路归零盲区；`price_variants` 条件变体首中即停（seq 升序，条件 AND 组合，效果为万分数整单倍率或绝对覆盖）。价格内部以**毫分**整数存储（1 USD = 100,000 毫分），**API 边界一律以 USD 正常值（float64）收发**——与 balance 同构：token 价内部 `math.Round(usd*1e11)`（USD/token ×1e6×1e5）/ 展示 `millis/1e5` per 1M tokens，image 每张/次价 `math.Round(usd*1e5)`；入参 `≥0`（负数 → 400），`nil` = 无该分量（回退）。

价格来源分两路，**行级互斥**：

- `litellm`：从 litellm 官方价格表拉取（`price_source_url` 配置的 JSON，默认 GitHub raw `model_prices_and_context_window.json`）。启动时异步拉取一次 + `price_sync_cron` 定时（默认 `0 3 * * *`）。批量 upsert，**永不覆盖已存在的手动价**（`ON CONFLICT (model) DO UPDATE ... WHERE source != 'manual'`；变体同步同理跳过手动模型的变体）。
- `manual`：管理端手动设价（PUT），**优先级最高**——upsert 强制 `source=manual`，可直接接管已存在的 litellm 行；手动变体由管理端全量替换（PUT variants）。

**mode 与分量校验（PUT /prices/entry）**：`mode=token` ⇒ `input_per_m` + `output_per_m` 必填；`mode=call` ⇒ `price_per_call` 必填；`mode=image` ⇒ image 三分量至少其一；其余分量可选配。`max_input_tokens` / `max_output_tokens` / `supports_prompt_caching` 为 litellm 元数据（仅 litellm 行携带，manual 行 `nil`；查询与计费无关，信息保留在 `raw` JSONB）。

**变体语义**：条件全可空=通配，多条件 AND；首条命中即停（按 seq 升序）。效果至少其一非空：`multiplier` 倍数（0..10，0=免费档，上限 10=×10；API 边界倍数小数——存储 `mult_bp` 万分：存储 15000 ↔ 显示 1.5）或 8 个绝对覆盖分量：`set_input_per_m`/`set_output_per_m`/`set_cache_read_per_m`/`set_cache_creation_per_m`（token 模式）/`set_price_per_call`（call 模式）/`set_img_in_tok_per_m`/`set_img_out_tok_per_m`/`set_price_per_image`（image 模式）；倍率作用于全分量，覆盖可指任一分量。DB 侧 `CHECK(mult_bp IS NOT NULL OR set_input_per_m IS NOT NULL OR set_output_per_m IS NOT NULL OR set_cache_read_per_m IS NOT NULL OR set_cache_creation_per_m IS NOT NULL OR set_price_per_call IS NOT NULL OR set_img_in_tok_per_m IS NOT NULL OR set_img_out_tok_per_m IS NOT NULL OR set_price_per_image IS NOT NULL)`（约束名 `price_variants_effect_at_least_one_v2`，幂等升级）防御空效果行。前端按条目 `mode` 渲染对应覆盖组。

**解析与计费**：`ResolveEntryPrices` 按 entry 基底 → 首中变体 → multiplier 倍率（0..10，存储换算万分钳制 [0,100000] 溢出防御）全体乘 → set_* 绝对覆盖；`billing.CostFromResolved` 纯算术对解析后单价组计算（零分配零锁；价格快照读零 DB）。

### 价格列表

`GET /api/admin/prices`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 页码，**1-based**；缺省或 `<1` 按 1 |
| `page_size` | int | 20 | 每页行数；越界（`<1` 或 `>100`）→ 400 |
| `mode` | string | — | 筛选：`token` / `call` / `image`；非法 → 400 |
| `source` | string | — | 筛选：`litellm` / `manual`；非法 → 400 |
| `provider` | string | — | 筛选：厂商等值匹配（如 `openai` / `anthropic` 等）；`NULL` 行在过滤时排除（标准语义——provider 列 nullable） |
| `model` | string | — | 模型名模糊搜索（大小写不敏感） |
| `sort` | string | `model` | 白名单：`model` / `updated_at`；非法 → 400 |
| `order` | string | `desc` | `asc` / `desc`；其他值 → 400 |

响应 `200`：`{"total": N, "rows": [PriceEntry...]}`（PriceEntry 含 mode 分量、provider/raw/source/时间；价格字段为 USD 正常值，`null` = 无该分量）。

### 价格条目 Entry

`GET /api/admin/prices/entry?model={model}` — 单行查询；`404` 模型不存在。

`PUT /api/admin/prices/entry?model={model}` — 手动设价（全量替换，body 含 `mode` + 各分量 USD 正常值；`model` 走 query 参数——模型名可含 `/`，路径参数单段匹配会拆段 404）。校验见上；upsert 强制 `source=manual`。响应 `200` 为更新后的 PriceEntry。

`DELETE /api/admin/prices/entry?model={model}` — 仅 `source=manual` 行可删（`200 {"deleted": true}`；litellm 行 `409`；不存在 `404`）。

### 价格变体 Variants

`GET /api/admin/prices/variants?model={model}` — 该模型变体集（seq 升序）。

`PUT /api/admin/prices/variants?model={model}` — 批量整体替换该模型变体集（请求体 `{"variants": [...]}`；空数组 = 清空）。每条 `seq` 唯一、效果至少其一（`multiplier` ∈ [0,10] 且 `set_*` ≥0，存储换算万分）；非法 → 400。响应 `200` 为替换后变体集。

`DELETE /api/admin/prices/variants?model={model}` — 清空该模型变体集（等价 PUT 空数组；`200 {"deleted": true}`）。

### 手动触发同步

`POST /api/admin/pricing/sync` — 手动触发一次价格拉取（与定时 worker 同路径：fetch → 批量 upsert → 快照重载，不等 cron）。响应 `200`：`{"rows": N, "skipped": M, "updated": K, "variants": V}`（`rows` 有效模型行数，`skipped` 非法行数，`updated` 入库数（manual 行不计），`variants` 变体数）。错误：`400` price_source_url 未配置；`502` 拉取上游失败（保留旧价格）；`500` 落库失败。

`POST /api/admin/pricing/sync/preview` — 预览拉取（零写库）：内存拉取+解析+与当前快照 diff → `{"to_add": N, "to_update": M, "skipped": K, "variants_changed": V, "entries": [...]}`（advisory，apply 重拉最新数据）。

### 相关 settings（PUT /api/admin/settings）

| key | 默认 | 说明 |
|---|---|---|
| `price_source_url` | litellm 官方价格表 JSON raw URL | 拉取源；空 → sync 拒绝（400） |
| `price_sync_cron` | `0 3 * * *` | 拉取 cron 表达式；变更下次循环生效 |
| `service_tier_policy_priority` | `passthrough` | 请求 `service_tier=priority` 的**转发策略**：`passthrough` / `strip` / `reject` |
| `service_tier_policy_flex` | `passthrough` | 同上，作用于 `service_tier=flex` 请求 |
| `service_tier_policy_fast` | `passthrough` | 同上，作用于 `service_tier=fast` 请求（Anthropic Fast Mode） |

> 策略仅影响**转发体**；计费读取不受影响（剥离/拒绝路径照常按 priority/flex/fast 档计价）。`auto`/空/未知 tier 恒透传。非法值（非三值）→ `400`。


---

## 计费 Billing

计费链路：请求前**预检**（价格快照缺价 / 余额快照 <0 → `402`；余额 0 放行——临时额度由 FEFO 扣费消化）→ 请求完成聚合计费（`internal/billing` 纯函数：tier 选价 + above 分段 + fast 倍率 + 价格倍率）→ 内存聚合、周期批量**条件扣费**（毫分直接扣减，零换算）→ 明细落 `usage_logs`（cost/tier/above_hit/overdraft 列）。

### 启用顺序（config.toml）

```toml
billing = { enabled = true, flush_interval = "250ms", balance_refresh_interval = "10s" }
```

| 配置 | 默认 | 说明 |
|---|---|---|
| `billing.enabled` | `true` | **默认开启**。首次提供流量前必须同步价格（`POST /api/admin/pricing/sync` 或等待定时拉取）——空价格表 = 全模型 402（契约语义：缺价不按 0 计价） |
| `billing.flush_interval` | `250ms` | 账本游标轮询周期：三车道语句化结算消费 `usage_logs` 中的未结算行；停机排空受 shutdown 预算约束，超时 Warn 截断、不阻塞退出 |
| `billing.balance_refresh_interval` | `10s` | 余额快照全量刷新周期（预检读快照；扣费后定向即时刷新该 user） |

关联配置：`proxy.usage_capture`（日志开关；billing 路由判定包含它）、`usage.log_retention_days` / `usage.errlog_retention_days` / `usage.stats_retention_days`（三表分区保留天数，见「日志与统计」）。

### 402 语义（计费拒绝）

| 场景 | 行为 |
|---|---|
| 模型无价格（价格表缺行 / 快照缺失） | 请求前预检 `402`，错误类型 `billing`，不计费不转发 |
| 余额快照缺失或 **< 0**（非免费用户/组） | 请求前预检 `402`（错误类型 `billing`）。**余额恰为 0 放行**——临时额度由 FEFO 扣费消化，预检不拦 |
| 余额不足（快照滞后导致预检通过） | 条件扣费允许**透支**（`balance` 可为负），日志 `overdraft = true` |
| 免费（用户/组倍率 0） | 预检放行且不扣费（请求仍须有价格） |
| 价格在请求处理中被删（竞态） | 运行时防御：`Warn` + 该请求计费 0（`billing_tier = "no_price"` 审计） |

### 扣费与明细

- **临时额度 FEFO**：未过期 `temp_balances` 按 `expires_at` 升序逐行扣至 0（最早到期先扣，永久额度最后），剩余扣 `users.balance`（数据面端点见「临时额度 Temp Balances」章节）。
- **key 额度后扣**：`keys.quota` / `quota_used` 同为毫分；请求热路径读内存预算快照，耗尽才回 DB 复核认领，实际累计由 Recorder 周期增量回写（内存权威，DB 滞后 ≤ `quota_flush_interval`）。无额度 key（`quota = 0`）恒不累计——回写 SQL 带 `WHERE "quota" > 0` 守卫。详见「密钥 Keys」。
- **全毫分直接扣减**：1 USD = 100,000 毫分，cost/balance/temp_balance/兑换码 Value 同单位，无换算无取整。
- **优雅停机**：SIGTERM → 2s 优雅窗口 → 强断长连接（在途流式按已累积 token 计费）→ 等在途归零 → 排空扣费（计费 flusher 最先排空，日志 cost 不丢）。崩溃丢 ≤ 1 flush 窗口（接受）。
- 管理面余额 API 均以 USD float64 输入/展示（换算见「用户 Users」章节）。

---

## 认证失败与错误码

| 状态码 | 场景 |
|---|---|
| `400` | 请求体非法 / 修改密码新密码为空或超 72 字节 / 路径 ID 非法 / 非法 `sort` 或 `order` / 非法 `status` 枚举 / 批量 `ids` 为空或超 100 条 / 批量 `fields` 为空 / 规则 `when`/`then` 校验失败 / 兑换码生成参数非法（`type` 非法、`value ≤ 0`、`temp_balance` 缺 `resource_expires_at`、`expires_at` 过去、`count` 越界）/ 兑换码无效（`invalid code`：不存在/失效/过期/用尽，统一不泄露细节）/ 价格负数或非负校验失败 / `fast_multiplier` 越界 / 倍率（组/用户-组专属 `price_multiplier`，正常值 `0`~`10`）越界 / `service_tier_policy_*` 非法值 / `source` 筛选非法 / `price_source_url` 未配置触发 sync |
| `401` | admin 凭据（静态 token 或 platform_admin JWT）缺失或错误；普通 `user` 角色 JWT 访问 `/api/admin/*` |
| `402` | **计费拒绝**（`error_type=billing`）：模型缺价 / 余额快照缺失或 ≤ 0（AI 请求面，非管理面） |
| `404` | 资源不存在（单资源与批量均返回，消息含缺失 id，如 `service: not found: id=999 missing`） |
| `409` | 规则 `priority`/`name` 唯一冲突 / 兑换码重复兑换（`already redeemed`）/ 删除 litellm 价格行 |
| `500` | 服务端错误（DB 等） |
| `502` | 价格同步拉取上游失败（保留旧价格） |

错误响应体统一为 `{"error": "<消息>"}`。
