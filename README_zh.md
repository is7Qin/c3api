<div align="center">

# ⚡ c3api

**轻量 AI 网关**——一个入口统一接入 OpenAI Responses API、Anthropic Messages API 与 OpenAI Chat Completions API，内置管理台、用量统计与计费。

[English](./README.md) | [中文](./README_zh.md)

[![Release](https://img.shields.io/github/v/release/is7Qin/c3api?color=2563eb)](https://github.com/is7Qin/c3api/releases)
[![Stars](https://img.shields.io/github/stars/is7Qin/c3api?color=2563eb)](https://github.com/is7Qin/c3api)
[![License](https://img.shields.io/badge/license-AGPL--3.0--or--later%20%2B%20Commercial-2563eb)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-2563eb)](https://go.dev/)

[![OpenAI Responses API](https://img.shields.io/badge/OpenAI%20Responses%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/responses)
[![Anthropic Messages API](https://img.shields.io/badge/Anthropic%20Messages%20API-✓-d97757)](https://docs.anthropic.com/en/api/messages)
[![OpenAI Chat Completions API](https://img.shields.io/badge/OpenAI%20Chat%20Completions%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/chat)

</div>

**c3api** 是自托管的 AI 网关，用一个统一入口对接多个上游供应商。它原生支持六种请求格式——OpenAI Responses API（含 WebSocket 变体）、Anthropic Messages API、OpenAI Chat Completions API、OpenAI Images API、Codex 网页搜索与 OpenAI 兼容模型列表——并映射到所配置的上游账号，提供模型路由、配额、用量计费与内嵌管理台。

## 状态：Beta

c3api 处于 **beta**：功能齐全，但破坏性变更自由。

- **不向后兼容**——数据库表结构与配置**不向后兼容**，且**不提供迁移路径**。
- **升级 = 全新建**——从早期版本升级需全新创建数据库、从零核对配置。
- 发布说明见 [CHANGELOG.md](./CHANGELOG.md)。

## 特性

| | |
|---|---|
| **六格式一网关** | OpenAI Responses API（REST + WebSocket）、Anthropic Messages API、OpenAI Chat Completions API、OpenAI Images API、Codex 网页搜索与 OpenAI 兼容模型列表——各自独立的上游编排与协议转换 |
| **模板与账号管理** | 模型模板、上游账号、分组、凭证，以及按模板的格式/模型白名单 |
| **管理台** | React 前端内嵌进二进制（`/admin`），另有 OpenAPI 契约定义的管理 API |
| **计费与用量** | 用户余额预检扣费、FEFO 临时额度、litellm 价格同步的按模型计费、按日分区的用量日志与统计——计费默认开启（`config.example.toml` billing.enabled=true） |
| **规则引擎** | 可自定义的路由、限流与 429/错误退避规则，内置调度器 |
| **多实例就绪** | 状态全在 PostgreSQL，`NOTIFY` 跨实例失效广播，水平扩容零配置 |
| **单二进制** | Go 二进制内嵌前端，非 root 容器镜像，即插即用部署 |

### 请求格式

| 格式 | 端点 | 上游 |
|---|---|---|
| OpenAI Responses API | `POST /v1/responses` | OpenAI Responses（REST） |
| OpenAI Responses API — WebSocket | `WS /v1/responses`（带 upgrade 头的 GET） | OpenAI Responses WebSocket（如 Codex 客户端） |
| Anthropic Messages API | `POST /v1/messages` | Anthropic Messages（REST + SSE） |
| OpenAI Chat Completions API | `POST /v1/chat/completions` | OpenAI Chat Completions（REST + SSE） |
| OpenAI Images API | `POST /v1/images/generations` / `POST /v1/images/edits` | OpenAI Images（JSON + multipart，REST + SSE） |
| Codex 网页搜索 | `POST /v1/alpha/search` | Codex SDK Search（仅 Codex 客户端） |
| OpenAI 模型列表 | `GET /v1/models` | 调度器内存快照（零 DB） |

## 快速开始

### 方式 A：Docker Compose（推荐）

**跑预构建镜像（pull）**——生产推荐：删除 `deploy/compose.yml` 里的 `build:` 块后 `up` 直接运行拉取的镜像：

```bash
cp .env.example .env        # 填写 ADMIN_TOKEN 与 AUTH_JWT_SECRET
docker compose --env-file .env -f deploy/compose.yml pull
docker compose --env-file .env -f deploy/compose.yml up -d
```

**自建镜像（build）**——compose 本地构建（`image` 与 `build` 并存时 `build` 优先）：

```bash
cp .env.example .env        # 填写 ADMIN_TOKEN 与 AUTH_JWT_SECRET
docker compose --env-file .env -f deploy/compose.yml up -d --build
```

网关监听 `http://127.0.0.1:18080`——管理台 `/admin`，健康检查 `/healthz`。

预构建镜像发布在 GHCR（`ghcr.io/is7qin/c3api`）：`:beta` 跟随最新 beta 版本，另有版本固定 tag（如 `:v0.0.1-beta.1`）。单独拉取：`docker pull ghcr.io/is7qin/c3api:beta`。

### 方式 B：本地开发

```bash
# 0. 注入本地开发密钥（config.toml 留空值；占位符如 change-me 会被 Load 拒绝）
export C3API_ADMIN_TOKEN=local-admin-token
export C3API_AUTH_JWT_SECRET=$(openssl rand -hex 16)

# 1. 启动网关（默认 :18080）
go run ./cmd/server -config config.toml

# 2. 启动前端 dev server（:5173，/admin 代理到 18080）
cd web && pnpm install && pnpm run dev
```

任意兼容 OpenAI/Anthropic 的 SDK 指向网关地址即可——请求格式由路径决定，一个 base URL 同时服务六种请求格式。

## 架构

```
                    ┌───────────────────────────────┐
 OpenAI SDK / curl ─▶│   c3api 网关（单二进制）        │
 Anthropic SDK ─────▶│  ┌─────────────────────────┐  │
 Codex 客户端 ──────▶│  │ chi 路由                │  │──▶ OpenAI 上游（REST + SSE）
   浏览器（SPA） ───▶│  │ /healthz /admin /user   │  │──▶ Anthropic 上游（REST + SSE）
                    │  │ /v1/*                    │  │──▶ Responses / resp-ws 上游
                    │  │ proxy：鉴权 → 门禁 →      │  │
                    │  │         选号 → 转发       │  │
                    │  └──────────┬──────────────┘  │
                    │   workers：billing / usage /  │
                    │   errlog / scheduler / notify │
                    │   retention / stats-agg /     │
                    │   pricing-sync / rule-engine  │
                    │   auth-sync / invalidate      │
                    └──────────────┼────────────────┘
                                   ▼
                        PostgreSQL 18（状态 + NOTIFY）
```

- **单二进制**：前端经 `go:embed` 内嵌，运行时 = 一个 `server` 进程 + 挂载的配置文件。
- **网关无状态、状态在 DB**：共享状态全部在 PostgreSQL，实例间经 `c3api_invalidate` 通道 `NOTIFY` 协调——加实例即扩容。
- **常驻 worker**：计费扣减、用量/统计落库、错误审计、分区保留、离线聚合、价格同步与规则调度均为长驻 worker，支持优雅停机排空。

## 配置

网关加载 `config.toml`（模板见 `config.example.toml`），`C3API_` 前缀环境变量可覆盖（前缀**必须大写**）：

| 变量 | 说明 |
|---|---|
| `C3API_ADMIN_TOKEN` | 管理端 token（必填） |
| `C3API_AUTH_JWT_SECRET` | 用户鉴权 JWT 密钥（必填；跨重启与多实例须稳定） |
| `C3API_DB_DSN` | PostgreSQL 连接串 |

完整配置项（server / log / admin / auth / db / proxy / upstream / limit / scheduler / usage / billing）见 `config.example.toml`。

- **仅全新部署（不适配迁移）**——表结构与配置跨版本不向后兼容：升级即全新创建数据库、从零重核对配置（见上方"状态：Beta"说明）。
- **纯 env 部署**（如 K8s）：传 `-config ""` 完全跳过配置文件——flag 默认 config.toml，无文件即启动失败。
- **配置仅启动时读取**，变更需滚动重启（无热更新）。
- **非法配置启动即报错**（含字段名）：duration/间隔 ≤0 或 <1ms、未知键（拼写错误/已移除旧键）、必填密钥缺失、占位符（change-me、dev-admin-token 等）一律拒绝。

## 部署

- `deploy/compose.yml` — 生产编排：`db`（postgres:18-alpine，目录挂载数据）+ `app`（单容器，非 root、配置只读挂载、健康检查）。
- `Dockerfile` — 三阶段构建（node → go → alpine），产出内嵌 UI 的静态单二进制。
- 不依赖外部缓存或消息中间件——PostgreSQL 是唯一依赖。

## 许可

c3api 以 **GNU AGPL v3.0-or-later**（`LICENSE`）开源——可自由使用、修改与部署，**包括商用与托管服务**，唯一义务是修改以相同条款回馈社区，**无需购买**。

需要**闭源部署**（豁免部署上的 AGPL 义务）？可获取商业授权（`LICENSE.commercial`）——付费获得对部署的 AGPL 义务豁免。

外部代码贡献需签署 CLA（贡献者版权转让协议），以便贡献代码并入本双许可体系——详见 [CLA.md](./CLA.md) 与 [CONTRIBUTING.md](./CONTRIBUTING.md)。

## 联系

- GitHub：[is7Qin/c3api](https://github.com/is7Qin/c3api)
- 问题：欢迎通过 GitHub issue 提交 bug、咨询或功能建议
- 安全：漏洞请经 [SECURITY.md](./SECURITY.md) 私有报告
- 发布说明：[CHANGELOG.md](./CHANGELOG.md)
