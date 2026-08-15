<div align="center">

# ⚡ c3api

**A lightweight AI gateway** — one entry point for the OpenAI Responses API, the Anthropic Messages API, and the OpenAI Chat Completions API, with a built-in admin console, usage tracking, and billing.

[English](./README.md) | [中文](./README_zh.md)

[![Release](https://img.shields.io/github/v/release/is7Qin/c3api?color=2563eb)](https://github.com/is7Qin/c3api/releases)
[![Stars](https://img.shields.io/github/stars/is7Qin/c3api?color=2563eb)](https://github.com/is7Qin/c3api)
[![License](https://img.shields.io/badge/license-AGPL--3.0--or--later%20%2B%20Commercial-2563eb)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-2563eb)](https://go.dev/)

[![OpenAI Responses API](https://img.shields.io/badge/OpenAI%20Responses%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/responses)
[![Anthropic Messages API](https://img.shields.io/badge/Anthropic%20Messages%20API-✓-d97757)](https://docs.anthropic.com/en/api/messages)
[![OpenAI Chat Completions API](https://img.shields.io/badge/OpenAI%20Chat%20Completions%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/chat)

</div>

**c3api** is a self-hosted AI gateway that fronts multiple upstream providers with one unified entry point. It speaks all six request formats — OpenAI Responses API (including its WebSocket variant), Anthropic Messages API, OpenAI Chat Completions API, OpenAI Images API, Codex web search, and an OpenAI-compatible model list — and maps them onto your configured upstream accounts with model routing, quotas, usage accounting, and an embedded admin console.

## Status: Beta

c3api is in **beta**: feature-complete, but breaking changes are free to happen.

- **Not backward-compatible** — database schemas and configuration are **not** backward-compatible across versions, and **no migration path** is provided.
- **Upgrade = fresh setup** — upgrading from an earlier version means provisioning a brand-new database and re-checking your configuration from scratch.
- See [CHANGELOG.md](./CHANGELOG.md) for release notes.

## Features

| | |
|---|---|
| **Six formats, one gateway** | OpenAI Responses API (REST + WebSocket), Anthropic Messages API, OpenAI Chat Completions API, OpenAI Images API, Codex web search, and the OpenAI-compatible model list — each with its own upstream orchestration and protocol conversion |
| **Template & account management** | Model templates, upstream accounts, groups, credentials, and per-template format/model allowlists |
| **Admin console** | React web UI embedded in the binary (`/admin`), plus a full OpenAPI-defined admin API |
| **Billing & usage** | Per-user balance with pre-check deductions, FEFO temporary quotas, per-model pricing synced from litellm, daily-partitioned usage logs and statistics — billing is enabled by default (`config.example.toml` billing.enabled=true) |
| **Rules engine** | Customizable routing, rate limiting, and 429/error backoff rules with a built-in scheduler |
| **Multi-instance ready** | PostgreSQL-based state, `NOTIFY`-based cross-instance invalidation, zero-config horizontal scaling |
| **Single binary** | Go binary with embedded frontend, non-root Docker image, drop-in deployment |

### Request formats

| Format | Endpoint | Upstream |
|---|---|---|
| OpenAI Responses API | `POST /v1/responses` | OpenAI Responses (REST) |
| OpenAI Responses API — WebSocket | `WS /v1/responses` (GET with upgrade header) | OpenAI Responses WebSocket (e.g. Codex client) |
| Anthropic Messages API | `POST /v1/messages` | Anthropic Messages (REST + SSE) |
| OpenAI Chat Completions API | `POST /v1/chat/completions` | OpenAI Chat Completions (REST + SSE) |
| OpenAI Images API | `POST /v1/images/generations` / `POST /v1/images/edits` | OpenAI Images (JSON + multipart, REST + SSE) |
| Codex web search | `POST /v1/alpha/search` | Codex SDK Search (Codex client only) |
| OpenAI model list | `GET /v1/models` | In-memory scheduler snapshot (zero DB) |

## Quick Start

### Option A: Docker Compose (recommended)

**Run the prebuilt image (pull)** — for production, remove the `build:` block in `deploy/compose.yml` so `up` runs the pulled image instead of building:

```bash
cp .env.example .env        # fill in AUTH_JWT_SECRET (ADMIN_TOKEN optional — see below)
docker compose --env-file .env -f deploy/compose.yml pull
docker compose --env-file .env -f deploy/compose.yml up -d
```

**Self-build** — compose builds the image locally (`image` and `build` coexist, `build` wins):

```bash
cp .env.example .env        # fill in AUTH_JWT_SECRET (ADMIN_TOKEN optional — see below)
docker compose --env-file .env -f deploy/compose.yml up -d --build
```

The gateway listens on `http://127.0.0.1:18080` — admin console at `/admin`, health check at `/healthz`.

**First admin user (bootstrap)** — there is no separate "create admin" step: the **first user to register** on a fresh database automatically becomes a `platform_admin`, so you can sign up and log into the admin console (`/admin`) right after startup, with or without `ADMIN_TOKEN`. Every later signup gets the regular `user` role. `ADMIN_TOKEN` (if set) keeps working for API clients (curl/scripts). In the narrow window where two signups race on an empty database, both may become admins — remove the extra account in the admin console if that happens.

Prebuilt images are published to GHCR (`ghcr.io/is7qin/c3api`): `:beta` tracks the latest beta release, and version-pinned tags such as `:v0.0.1-beta.1` are also available. Pull standalone: `docker pull ghcr.io/is7qin/c3api:beta`.

### Option B: Local development

```bash
# 0. Inject local dev secrets once (config.toml keeps empty values; placeholder
#    values like change-me are rejected by config.Load)
export C3API_ADMIN_TOKEN=local-admin-token
export C3API_AUTH_JWT_SECRET=$(openssl rand -hex 16)

# 1. Start the gateway (default :18080)
go run ./cmd/server -config config.toml

# 2. Start the frontend dev server (:5173, proxies /admin to 18080)
cd web && pnpm install && pnpm run dev
```

Point any OpenAI/Anthropic-compatible SDK at the gateway URL — the request format is selected by path, so a single base URL serves all six request formats.

## Architecture

```
                    ┌───────────────────────────────┐
 OpenAI SDK / curl ─▶│   c3api gateway (1 binary)    │
 Anthropic SDK ─────▶│  ┌─────────────────────────┐  │
 Codex client ──────▶│  │ chi router              │  │──▶ OpenAI upstream (REST + SSE)
   Browser (SPA) ───▶│  │ /healthz /admin /user   │  │──▶ Anthropic upstream (REST + SSE)
                    │  │ /v1/*                    │  │──▶ Responses / resp-ws upstream
                    │  │ proxy: auth → gate →     │  │
                    │  │         route → forward  │  │
                    │  └──────────┬──────────────┘  │
                    │   workers: billing / usage /  │
                    │   errlog / scheduler / notify │
                    │   retention / stats-agg /     │
                    │   pricing-sync / rule-engine  │
                    │   auth-sync / invalidate      │
                    └──────────────┼────────────────┘
                                   ▼
                         PostgreSQL 18 (state + NOTIFY)
```

- **Single binary**: the frontend is built and embedded via `go:embed`, so the runtime is one `server` process plus a mounted config file.
- **Stateless gateway, stateful DB**: all shared state lives in PostgreSQL; instances coordinate through `NOTIFY` on the `c3api_invalidate` channel — scale horizontally by just adding instances.
- **Persistent workers**: billing deduction, usage/statistics flushing, error-log auditing, partition retention, offline stats aggregation, price sync, and the rule scheduler run as long-lived workers with graceful shutdown draining.

## Configuration

The gateway loads `config.toml` (see `config.example.toml`), overlaid by `C3API_`-prefixed environment variables (the prefix must be **uppercase**):

| Variable | Description |
|---|---|
| `C3API_ADMIN_TOKEN` | Admin API token (optional; leave empty to disable static-token auth — `/admin` then accepts `platform_admin` JWTs only) |
| `C3API_AUTH_JWT_SECRET` | JWT signing secret for user auth (required; stable across restarts and instances) |
| `C3API_DB_DSN` | PostgreSQL DSN |

See `config.example.toml` for the full schema (server, log, admin, auth, db, proxy, upstream, limit, scheduler, usage, billing).

- **Fresh setup only (no migration path)** — schemas and configuration are not backward-compatible between versions: an upgrade means a brand-new database and a re-checked configuration (see [Status: Beta](#status-beta)).
- **Env-only deployments** (e.g. K8s): pass `-config ""` to skip the config file entirely — the flag defaults to `config.toml`, and a missing file is a startup error.
- **Config is read once at startup** — changes require a rolling restart (no hot reload).
- **Invalid config fails fast at startup** with the offending key: non-positive durations/intervals, unknown keys (typos, removed legacy keys), missing required secrets, and placeholder values (`change-me`, `dev-admin-token`, …) are all rejected.

## Deployment

- `deploy/compose.yml` — production stack: one `db` (postgres:18-alpine, bind-mounted data) + one `app` container (non-root, read-only config mount, healthcheck).
- `Dockerfile` — three-stage build (node → go → alpine), producing a single static binary with the UI embedded.
- No external caching or message-bus service is required — PostgreSQL is the only dependency.

### GC tuning (optional, default off)

Under high concurrency (25k+ concurrent streams) the default Go GC becomes a measurable cost. A/B-tested at 50k concurrency on a 24-core box, `GOGC=off` + `GOMEMLIMIT=17179869184` (16 GiB — plain bytes, the env var rejects unit suffixes like `16G`) measured:

- Throughput **+14.5%** (25.1k → 28.75k req/s)
- Per-request CPU **-27.7%** (331 → 239 µs)
- First-byte latency **-21%** (1103 → 873 ms); p99 first-byte **-36%** (5785 → 3705 ms)

Trade-off: the heap grows to the limit (~13 GiB with a 16 GiB cap) and GC fires roughly every ~10 s instead of every ~2-3 s — fine on a 64 GiB box; raise `GOMEMLIMIT` to cut the frequency further at the cost of memory. Set both in `.env` (empty values keep Go defaults):

```env
GOGC=off
GOMEMLIMIT=17179869184
```

## License

c3api is open source under the **GNU AGPL v3.0-or-later** (`LICENSE`) — free to use, modify, and deploy, **including for commercial and hosted services**, with the single obligation that modifications are contributed back under the same terms. **No purchase is required.**

Need **closed-source deployment** (exempt from the AGPL obligations on your deployment)? A commercial license (`LICENSE.commercial`) is available — it waives the AGPL obligations for your deployment.

External code contributions require a CLA (contributor license agreement) so that contributed code can be merged under this dual-license scheme — see [CLA.md](./CLA.md) and [CONTRIBUTING.md](./CONTRIBUTING.md).

## Contact

- GitHub: [is7Qin/c3api](https://github.com/is7Qin/c3api)
- Issues: open a GitHub issue for bugs, questions, or feature requests
- Security: report vulnerabilities via [SECURITY.md](./SECURITY.md) (private report)
- Changelog: [CHANGELOG.md](./CHANGELOG.md)
