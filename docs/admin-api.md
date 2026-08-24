# Admin API 文档

管理端 API（配置模板 / 账号 / 分组 / 兑换码 / 模型价格 / 日志 / 统计 / 临时额度）。与 AI 推理请求（`/v1/*`，模型请求）相对，本组接口为网关管理面，不对上游转发。

## 通用约定

- **Base URL**：`http://<gateway>/api/admin`
- **认证**：所有请求必须带 `Authorization: Bearer <admin_token>`（`config.toml` 的 `admin.token`，或环境变量 `C3API_ADMIN_TOKEN`）。缺失或错误返回 `401`。
- **Content-Type**：请求体与响应均为 `application/json`（`rotate-key` 等无请求体操作除外）。
- **错误格式**：非 2xx 响应体为 `{"error": "<消息>"}`。404 的消息含缺失资源 id（如 `service: not found: id=999 missing`），便于定位。
- **ID**：路径参数 `{id}` 为模板/账号/分组的整数 ID。
- **更新语义**：`PUT` 为**全量替换**——请求体中的字段整体覆盖，未提供的字段清零（仅提供部分字段的 `PUT` 会把缺失字段重置为空/零值）。批量 `batch-update` 为**部分更新**（只改 `fields` 中提供的字段）。
- **列表响应**：templates / accounts / groups 三个旧端点统一返回 `{"total": <满足筛选的总数>, "rows": [...]}`，支持 `limit` / `offset` 分页、筛选参数与白名单 `sort` / `order` 排序（非法 `sort` / `order` → `400`）。兑换码与模型价格为**增强分页范式**（`page` / `page_size`，1-based），见对应章节。

## 枚举值

| 枚举 | 取值 |
|---|---|
| `format`（请求格式） | `openai-chat` / `openai-responses` / `anthropic` |
| `status`（账号） | `active` / `unhealthy` / `429` / `disabled` |
| `error_type`（日志） | `none` / `429` / `4xx` / `5xx` / `network` / `auth` / `no_account` / `abort` / `billing`（计费拒绝 402） |
| `type`（兑换码） | `balance`（充值余额，面值 USD）/ `concurrency`（加并发数，面值整数）/ `temp_balance`（临时余额，面值 USD，兑换后资源到期） |
| `status`（兑换码） | `active` / `disabled`（不可编辑，仅可失效） |
| `source`（模型价格） | `litellm`（官方价格表拉取）/ `manual`（管理端手动设价，优先级最高） |

---

## 模板 Templates

模板定义上游厂商：base_url、支持的请求格式集合、可服务模型集合、格式级模型覆盖与模型映射。

> **破坏性变更**：`default_format` 已移除（由必填的 `supported_formats` 数组取代），`model_formats` 已移除（由反转为按格式组织的 `format_models` 取代）。旧数据未迁移，使用旧字段的客户端需按下方新结构调整。

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
  "model_mapping": { "gpt-4o": "gpt-4o-2024-11-20" }
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 模板名 |
| `base_url` | string | ✅ | 上游**根**地址（**不含 `/v1`**——`/v1` 是协议细节，网关按格式追加：openai 系拼 `/v1/...`，anthropic SDK 自带 `v1` 前缀；含尾 `/v1` 会被拒（`400`）） |
| `supported_formats` | string[] | ✅ | 支持的请求格式枚举数组（至少 1 项，项枚举见上；重复/非法枚举返回 `400`） |
| `models` | string[] | 否 | 可服务模型名集合 |
| `format_models` | object | 否 | 格式级模型覆盖：`{格式: [模型名]}`，key 必须是 `supported_formats` 子集、模型必须是 `models` 子集（否则 `400`）；未配置的格式 = 全部 `models` |
| `model_mapping` | object | 否 | 模型映射：`{客户端模型名: 上游实际模型名}` |

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
  "ModelMapping": { "gpt-4o": "gpt-4o-2024-11-20" },
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

> 注意：响应字段为 **Go 默认大写命名**（`ID` / `Name` / `BaseURL`…），请求字段为 snake_case。`Models` / `FormatModels` / `ModelMapping` 为 `null` 时表示空。

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
| `base_url` | string | 否 | 上游地址（合法 URL） |
| `supported_formats` | string[] | 否 | 支持的请求格式枚举数组（至少 1 项，枚举见上；重复/非法枚举 → `400`） |
| `models` | string[] | 否 | 可服务模型集合 |
| `format_models` | object | 否 | 格式级模型覆盖：`{格式: [模型名]}`，key 必须是 `supported_formats` 子集（同批提供时校验）、模型必须是 `models` 子集 |
| `model_mapping` | object | 否 | 模型映射 |

`fields` 必须至少提供一字段；`ids` 中任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 2}`。

### 模板其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /api/admin/templates/{id}` | 单个模板 | `200`：模板对象；`404` 不存在 |
| `PUT /api/admin/templates/{id}` | 全量更新（字段同创建） | `200`：更新后模板对象 |
| `DELETE /api/admin/templates/{id}` | 删除 | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id）；仍被账号引用时返回 `500`（DB 外键约束） |

> 模板变更（含 base_url / supported_formats / format_models / model_mapping）通过 invalidate 回调即时生效于调度器快照与上游 SDK 客户端（无需重启）。

---

## 账号 Accounts

账号绑定模板并持有上游 API key，是调度的基本单元。

### 创建账号

`POST /api/admin/accounts`

```json
{
  "name": "a1",
  "template_id": 1,
  "upstream_key": "sk-xxx",
  "status": "active",
  "weight": 100,
  "max_concurrency": 8
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 账号名 |
| `template_id` | int | ✅ | 所属模板 ID |
| `upstream_key` | string | ✅ | 上游 API key（发送给上游鉴权） |
| `status` | string | 否 | 初始状态，默认 `active` |
| `weight` | int | 否 | 选号权重（预生成加权序列，权重比决定命中比例），默认 0 |
| `max_concurrency` | int | 否 | 账号并发上限；`0` 时使用调度器 `default_max_concurrency` |

响应 `200`：账号对象（含嵌套 `Template`）。

### 账号对象结构（响应）

```json
{
  "ID": 1,
  "Name": "a1",
  "TemplateID": 1,
  "Template": { "...": "嵌套模板对象" },
  "UpstreamKey": "sk-xxx",
  "Status": "active",
  "CooldownUntil": null,
  "Weight": 100,
  "MaxConcurrency": 8,
  "LastError": null,
  "LastUsedAt": null,
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

### 账号状态机

| 状态 | 进入条件 | 恢复 |
|---|---|---|
| `active` | 创建/成功请求 | — |
| `unhealthy` | 上游 5xx / 连接级错误 / 流中断 | 指数退避冷却（5s×2ⁿ，上限 5min）后自动恢复 |
| `429` | 上游 429 | 固定冷却（默认 30s）后自动恢复 |
| `disabled` | 管理端手动设置；或 SDK 凭据判死（codex-oauth/codex-pat 账号，`failed_at` 落库 + 调度摘除） | 手动改回（`PUT`） |

> `unhealthy` / `429` 为**健康轴**（自动退避），`disabled` 为**启用轴**（手动）。管理端 `PUT` 设 `disabled` 后，在途请求完成不会覆盖回写（防复活守卫）。

> **失效恢复须知（SDK 接入账号——codex-oauth / codex-pat）**：凭据被判死（refresh token 判死、token 端点 401、账号禁用等 SDK 判定终止）后账号置 `disabled` 摘除并写 `failed_at`。管理面将 status 改回 `active`（隐含清 `failed_at` / `last_error`）**只恢复"可调度"**——若凭据确已判死，请求面仍恒失败（适配层毒化凭据保留至凭据变更重建，失败会再次触发摘除）。**恢复须重新导入凭据**（`PUT /api/admin/accounts/{id}/ext` 更新 account_ext 后凭据签名变化 → 适配层重建）才可真正服务——这是有意设计：判死凭据不得在未重新导入的情况下复活。

### 账号列表（含运行时视图）

`GET /api/admin/accounts`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数 |
| `offset` | int | 0 | 分页偏移 |
| `sort` | string | `id` | 白名单：`id` / `name` / `template_id` / `status` / `cooldown_until` / `weight` / `max_concurrency` / `last_used_at` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |
| `status` | string | — | 多值过滤，逗号分隔（如 `active,disabled`）；非法枚举 → `400` |
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

请求体：`{"ids": [1], "fields": {"status": "disabled", "weight": 50}}`。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 否 | 账号名（非空） |
| `template_id` | int | 否 | 所属模板 ID（>0） |
| `upstream_key` | string | 否 | 上游 API key（非空） |
| `status` | string | 否 | 状态枚举（见上；非法枚举 → `400`） |
| `weight` | int | 否 | 选号权重（≥0） |
| `max_concurrency` | int | 否 | 并发上限（≥1） |

`fields` 必须至少提供一字段；`ids` 中任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 1}`。

### 账号其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /api/admin/accounts/{id}` | 单个账号 | `200`：账号对象；`404` 不存在 |
| `PUT /api/admin/accounts/{id}` | 全量更新（字段同创建；`status` 可改为 `disabled` 禁用） | `200`：更新后账号对象 |
| `DELETE /api/admin/accounts/{id}` | 删除 | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id） |

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
| `price_multiplier` | number / null | 否 | **价格倍率**（正常值：`1` = ×1、`0` = 免费、上限 `10` = ×10；API 边界与万分数换算——内部存储恒万分数）。缺省/`null` = 不设置（×1）；**显式 `0` = 免费组（创建路径即可设置，T3.5 修正）**。超界 → `400` |

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

> **价格倍率按组（T3.5 修正）**：用户本体无倍率字段——专属倍率挂在该用户与组的授予关系上（`PUT /api/admin/groups/{id}/assignments` 的 `multipliers`），用户在不同组可有不同倍率。

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
- `GET /user/stats`：我的用量统计（强制 `user_id` = 当前用户，防越权；字段与 `/api/admin/stats` 同契约，见「查询用量统计」章节）。
- `GET /user/temp-balances`：我的临时额度（仅有效额度：未过期且正余额，`expires_at` 升序 FEFO 同序、永久最后；`total_usd` 合计 USD），见「临时额度 Temp Balances」章节。
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
- **模板系统**：两套内置中文默认模板（注册验证码/重置密码验证码，占位符 `{{code}}` / `{{ttl_minutes}}` / `{{app_name}}`），管理台可编辑覆盖、清空正文即还原默认。

### 邮件模板管理（platform_admin）

- `GET /api/admin/mail/templates`：列出全部用途模板（缺行自动合成内置默认，恒返回 register_code/reset_code 两条）。
- `PUT /api/admin/mail/templates/{purpose}`：body `{subject, body_text}`；`body_text` 置空 = 删行还原内置默认。非法 purpose → 非 200。

SMTP 连接参数（host/port/username/password/from/tls）同为运行时设置键 `mail.*`，经通用 `GET|PUT /api/admin/settings` 读写（`smtp_password` 明文回读，沿用 `upstream_key` 先例；前端以密码框呈现）。

### 价格倍率语义（计费生效）

计费倍率作用在**整单计费成本**上（含 fast 倍率之后）：`cost = round(cost × mult / 10000)`（round-half-up），取数顺序为**用户-组专属覆盖组**（T3.5 修正：专属倍率按组挂载）：

1. `group_assignments.price_multiplier` 已设置（非 null，该用户在该组）→ 用户-组专属倍率；
2. 否则 `groups.price_multiplier`（组默认 `10000` = ×1）；
3. 两者均未设置 → ×1 原价。

`0` = **免费**（cost = 0 不扣费；请求仍须有价格，否则 402）；上限 `100000` = ×10。倍率预检：免费用户/组余额为 0 不 402。

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
| `401` | admin token 缺失或错误；普通 `user` 角色 JWT 访问 |

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

**计费字段**（Phase 5）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Cost` | int64 | 计费成本（**毫分**，1 USD = 100,000 毫分）；放行路径行可为 0（免费分组/0 token） |
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

`GET /api/admin/stats?from=2026-08-06T00:00:00Z&to=2026-08-06T23:59:59Z&granularity=day&group_id=1&account_id=2&model=gpt-4o`（管理侧，可 `user_id` 过滤）

`GET /user/stats?...`（用户侧，强制 `user_id` = 当前用户，`user_id` 过滤参数无效——防越权）

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `from` / `to` | RFC3339 | 近 24 小时 | 时间范围 |
| `granularity` | `hour` / `day` | `day` | 聚合粒度（`day` 为 UTC 日对齐，`hour` 为 UTC 小时对齐） |
| `group_id` / `account_id` / `template_id` / `user_id`(仅 `/api/admin/stats`) / `model` | int / int / int / int / string | — | 维度过滤 |

响应 `200`：统计行数组（按粒度对齐的桶）：

```json
[
  {
    "BucketTime": "2026-08-06T00:00:00Z",
    "GroupID": 1,
    "AccountID": 2,
    "TemplateID": 1,
    "Model": "gpt-4o",
    "IsError": false,
    "RequestCount": 100,
    "ErrorCount": 0,
    "InputTokens": 1000,
    "OutputTokens": 2000,
    "TotalTokens": 3000,
    "CacheReadTokens": 100,
    "CacheCreationTokens": 0,
    "Cost": 0.5,
    "CallCount": 12,
    "TTFTCount": 90,
    "TTFTAvgMS": 620.5,
    "TTFTMaxMS": 3800,
    "TTFTP50MS": 500,
    "TTFTP90MS": 1500,
    "TTFTP95MS": 2100,
    "TTFTP99MS": 3400
  }
]
```

字段说明：

- `Cost`（float64 **USD**）= 内部毫分 /1e5，与价格 API、`/api/admin/overview` 口径一致（破坏性变更：旧版为毫分 int64）
- `CallCount` = 按次调用计数（图片生成张数 / search 次数；**不入** `TotalTokens`）
- `TTFT*` = 首 token 时间（毫秒）统计，**仅含首 token 流式请求**（非流式/失败/无首 token 行不计）：`TTFTCount` 样本数（pN/加权 avg 分母）、`TTFTAvgMS` = ΣTTFT/样本数、`TTFTMaxMS` 最大值、`TTFTP50/P90/P95/P99MS` = 直方图插值分位数（nearest-rank + 桶内线性插值；顶桶 `[12800ms, ∞)` 回落 12800；无样本全 0）
- 前端跨行合并语义：avg 加权（`Σ(avg×count)/Σcount`）、max 取最大、**pN 取请求量最大维度行的近似值**（分位数不可跨行合并）
- 统计由**离线聚合 worker** 每 5 分钟从 `usage_logs`/`err_logs` 重算落盘（watermark + 覆盖语义），查询结果可能有 ≤5 分钟延迟；错误桶 = abort 行（usage_logs 全字段）+ 纯错误行（err_logs count 语义，tokens/cost/TTFT 恒 0）；拒绝行（限流）随 err_logs 采样——风暴时错误计数可能低估

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

- `summary`：今日汇总（UTC 日界），`cost_usd` 为 USD（毫分 /1e5），`ttft_*` 口径同 `/api/admin/stats`
- `trend`：近 N 天日桶（SQL 侧按日聚合；`tokens` = input+output+cache 合并）
- `accounts`：账号健康分布 + 并发水位（**调度器快照同源**——与账号列表运行时视图一致；运行时状态只在内存，DB 无第二份）
- `err_top`：账号维度错误率 Top5（调度器 EWMA，`name` = 账号名）
- `alerts`：billing 游标积压观测（lag 族——`billing_lag_ms` 时滞毫秒 / `billing_unbilled_rows` 未扣费行数 / `billing_quarantined_rows` 隔离行数累计）
- 聚合结果内部 TTL 30s 缓存（键含 `days`/`group_id` 与 UTC 日界）；统计本身为离线聚合产物（≤5 分钟陈旧）

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

`GET /ops/workers`——worker 运行状态（billing / invalidate / notify / pricing / retention / usage / **stats-agg** 等，各 worker `Stats()` 原样输出）。`stats-agg`（离线聚合 worker）四字段：

| 字段 | 说明 |
|---|---|
| `watermark_unix_ms` | 聚合水位（毫秒；0 = 未初始化/首轮未完成） |
| `last_buckets` | 上轮写入桶数（失败轮保留上轮值） |
| `last_rows` | 上轮消费明细行数（三查询合计） |
| `last_duration_ms` | 上轮耗时（毫秒） |

---

## 规则 Rules

规则引擎驱动账号状态管理（替代旧硬编码状态机）：请求结果事件按 `priority` 升序逐规则首中匹配，命中即执行 `then` 动作（状态/冷却/权重）。规则变更（增删改）即时生效（自动触发引擎重载）。

### 规则模型

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 规则名（唯一） |
| `priority` | int | 优先级（唯一，升序匹配、首中即停） |
| `enabled` | bool | 缺省 `true`；`false` = 停用（不参与匹配） |
| `when` | object | 匹配条件（字段白名单，未知字段拒绝 400） |
| `then` | object | 动作（`status`/`cooldown`/`weight` 至少一个） |

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

`then` 字段（至少一个）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `status` | string | `active` / `unhealthy` / `429` / `disabled` |
| `cooldown` | string | 冷却时长（`time.ParseDuration` 可解析且 >0，如 `"30s"`、`"5h"`） |
| `weight` | int | 权重（0-100，变更立即重建该组选号序列） |

种子规则（规则表为空时启动自动写入）：`kind=429 → status=429 + cooldown 30s`（priority 10）、`kind=4xx + http_status=400 → transmit`（priority 15，400 透传原文）、`kind=5xx → status=unhealthy + cooldown 10m`（priority 20）、`kind=network → status=unhealthy + cooldown 5s`（priority 25，连接级独立冷却）、`kind=ok → status=active`（priority 30，无冷却）。删除全部规则后，下次引擎重载（任意规则 CRUD 或重启）会自动重新播种——规则表不会保持真空。

### 事件模型与匹配语义

- **事件来源**：每次请求结算（MarkResult）产生一个事件 `{kind, http_status, error_message, account_id, template_id, group_id, model, occurred_at, reset_at}`，经有界队列投递规则 worker 匹配（队列满时丢弃并告警，不阻塞请求路径）。
- **条件投递**：仅当规则集中存在 `when.kind` 为 `nil`（任意）或 `ok` 的规则时，`ok` 事件才进入匹配；否则 ok 事件直接被跳过（性能优化）。想用 ok 事件恢复状态，必须保留 kind=ok 或全匹配规则（种子规则自带 ok 规则）。
- **首中即停**：按 `priority` 升序逐规则匹配，首个命中即执行其 `then` 全部动作，不再继续。
- **命中不清零窗口**：计数窗口为滑动窗口，命中不重置计数（自然衰减）；统计窗口固定粒度近似，误差 ≤ 一个粒度。
- **冷却语义**：命中且 `then.cooldown` 提供 → `cooldown_until = 命中时刻 + cooldown`。`then` 只设 status 无 cooldown 时，事件自带 `reset_at`（如上游 `Retry-After`）作为冷却兜底。
- **OK 不清除冷却**：`ok` 事件命中只恢复状态（如 `active`），**不**清除既有的 `cooldown_until`——调度器在冷却期内仍抑制该账号，避免 429 风暴后立即被打满。
- **状态变更即时生效**：规则增删改自动触发引擎重载；`weight` 动作立即重建对应组的选号序列（无需等 sync 周期）。

### 创建规则

`POST /api/admin/rules`

```json
{
  "name": "escalate-on-5xx",
  "priority": 40,
  "enabled": true,
  "when": { "kind": "5xx", "count_failure_ge": 5, "window_seconds": 60 },
  "then": { "status": "unhealthy", "cooldown": "30s" }
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
| `400` | `when`/`then` 含未知字段、`kind` 非法、计数为负、`window_seconds` < 1、比例越界或缺 `count_total_ge`、`then` 无动作、`cooldown` 非法、`weight` 越界 |
| `409` | `priority` 或 `name` 唯一冲突 |

> 配置变更：`scheduler.cooldown_429` / `scheduler.backoff_base` / `scheduler.backoff_max` **已移除**（2026-08-13 用户裁决：不向后兼容）——配置含这些键将启动失败（未知键报错）。429 冷却、错误退避与恢复节奏统一由规则引擎的种子规则与自定义规则接管。

## 兑换码 Redemption Codes

兑换码是资源发放的通用载体（Phase 5 计费前基础设施）：生成一批码 → 分发给用户 → 用户在 `/user/redemptions` 兑换 → 资源按码类型即时生效。管理面 5 个端点 + 用户面 2 个端点。

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

**变体语义**：条件全可空=通配，多条件 AND；首条命中即停（按 seq 升序）。效果至少其一非空：`multiplier` 倍数（0..10，0=免费档，上限 10=×10；API 边界倍数小数——存储 `mult_bp` 万分：存储 15000 ↔ 显示 1.5），`set_input_per_m`/`set_output_per_m` 绝对覆盖（仅 input/output 受覆盖；cache 分量只受 multiplier 影响）。DB 侧 `CHECK(mult_bp IS NOT NULL OR set_input_per_m IS NOT NULL OR set_output_per_m IS NOT NULL)` 防御空效果行。

**解析与计费**：`ResolveEntryPrices` 按 entry 基底 → 首中变体 → multiplier 倍率（0..10，存储换算万分钳制 [0,100000] 溢出防御）全体乘 → set_* 绝对覆盖；`billing.CostFromResolved` 纯算术对解析后单价组计算（零分配零锁；价格快照读零 DB）。

### 价格列表

`GET /api/admin/prices`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 页码，**1-based**；缺省或 `<1` 按 1 |
| `page_size` | int | 20 | 每页行数；越界（`<1` 或 `>100`）→ 400 |
| `mode` | string | — | 筛选：`token` / `call` / `image`；非法 → 400 |
| `source` | string | — | 筛选：`litellm` / `manual`；非法 → 400 |
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

Phase 5 计费链路：请求前**预检**（价格快照缺价 / 余额快照 <0 → `402`；余额 0 放行——临时额度由 FEFO 扣费消化）→ 请求完成聚合计费（`internal/billing` 纯函数：tier 选价 + above 分段 + fast 倍率 + 价格倍率）→ 内存聚合、周期批量**条件扣费**（毫分直接扣减，零换算）→ 明细落 `usage_logs`（cost/tier/above_hit/overdraft 列）。

### 启用顺序（config.toml）

```toml
billing = { enabled = true, flush_interval = "1s", balance_refresh_interval = "10s", flush_workers = 8 }
```

| 配置 | 默认 | 说明 |
|---|---|---|
| `billing.enabled` | `false` | **默认关闭（opt-in）**。启用前必须先同步价格（`POST /api/admin/pricing/sync` 或等待定时拉取）——空价格表 = 全模型 402（契约语义：缺价不按 0 计价） |
| `billing.flush_interval` | `1s` | 扣费批量落库周期（内存聚合满或周期到 → 逐 user 小事务条件扣费；停机排空受 shutdown 预算约束，超时 Warn 截断、不阻塞退出） |
| `billing.balance_refresh_interval` | `10s` | 余额快照全量刷新周期（预检读快照；扣费后定向即时刷新该 user） |
| `billing.flush_workers` | `8` | 并行落库 worker 数（O1 管道化分片并行，同用户实例内恒串行）。默认 8（多角度压测 C 项实证 2026-08-12：多用户水线告警清零、pending 斜率降 25-40%、QPS/CPU 零回退）：每 worker 一笔 `DeductAndLog` 事务，须 ≤ DB 连接池余量（`db.max_conns` 扣除其他连接占用） |

关联配置：`proxy.usage_capture`（日志开关；billing 路由判定包含它）、`usage.log_retention_days` / `usage.errlog_retention_days` / `usage.stats_retention_days`（三表分区保留天数，见「日志与统计」）。

### 402 语义（计费拒绝）

| 场景 | 行为 |
|---|---|
| 模型无价格（价格表缺行 / 快照缺失） | 请求前预检 `402`，错误类型 `billing`，不计费不转发 |
| 余额快照缺失或 ≤ 0（非免费用户/组） | 请求前预检 `402`（错误类型 `billing`） |
| 余额不足（快照滞后导致预检通过） | 条件扣费允许**透支**（`balance` 可为负），日志 `overdraft = true` |
| 免费（用户/组倍率 0） | 预检放行且不扣费（请求仍须有价格） |
| 价格在请求处理中被删（竞态） | 运行时防御：`Warn` + 该请求计费 0（`billing_tier = "no_price"` 审计） |

### 扣费与明细

- **临时额度 FEFO**：未过期 `temp_balances` 按 `expires_at` 升序逐行扣至 0（最早到期先扣，永久额度最后），剩余扣 `users.balance`（数据面端点见「临时额度 Temp Balances」章节）。
- **全毫分直接扣减**：1 USD = 100,000 毫分，cost/balance/temp_balance/兑换码 Value 同单位，无换算无取整。
- **优雅停机**：SIGTERM → 2s 优雅窗口 → 强断长连接（在途流式按已累积 token 计费）→ 等在途归零 → 排空扣费（计费 flusher 最先排空，日志 cost 不丢）。崩溃丢 ≤ 1 flush 窗口（接受）。
- 管理面余额 API 均以 USD float64 输入/展示（换算见「用户 Users」章节）。

---

## 认证失败与错误码

| 状态码 | 场景 |
|---|---|
| `400` | 请求体非法 / 修改密码新密码为空或超 72 字节 / 路径 ID 非法 / 非法 `sort` 或 `order` / 非法 `status` 枚举 / 批量 `ids` 为空或超 100 条 / 批量 `fields` 为空 / 规则 `when`/`then` 校验失败 / 兑换码生成参数非法（`type` 非法、`value ≤ 0`、`temp_balance` 缺 `resource_expires_at`、`expires_at` 过去、`count` 越界）/ 兑换码无效（`invalid code`：不存在/失效/过期/用尽，统一不泄露细节）/ 价格负数或非负校验失败 / `fast_multiplier` 越界 / 倍率（组/用户-组专属 `price_multiplier`，正常值 `0`~`10`）越界 / `service_tier_policy_*` 非法值 / `source` 筛选非法 / `price_source_url` 未配置触发 sync |
| `401` | admin token 缺失或错误；普通 `user` 角色 JWT 访问 `/api/admin/*` |
| `402` | **计费拒绝**（`error_type=billing`）：模型缺价 / 余额快照缺失或 ≤ 0（AI 请求面，非管理面） |
| `404` | 资源不存在（单资源与批量均返回，消息含缺失 id，如 `service: not found: id=999 missing`） |
| `409` | 规则 `priority`/`name` 唯一冲突 / 兑换码重复兑换（`already redeemed`）/ 删除 litellm 价格行 |
| `500` | 服务端错误（DB 等） |
| `502` | 价格同步拉取上游失败（保留旧价格） |

错误响应体统一为 `{"error": "<消息>"}`。
