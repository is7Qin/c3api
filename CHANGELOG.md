# Changelog

All notable changes to c3api are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versioning follows the policy below.

## Versioning

During the **beta** phase, versions are `v0.x.0-beta.N` (N increments with each release). The first beta is `v0.0.1-beta.1`; concrete numbers for later releases are decided at tag time.

## [v0.0.1-beta.2] - 2026-08-15

### Added

- HTTP `/responses` metadata alignment with the real codex client: per-request `turn_id` (UUIDv7) plus the static identity key set injected on the HTTP path, with passthrough short-circuit when the request already carries `client_metadata`.
- `x-codex-turn-state` response-header capture and same-turn replay on the HTTP path (turn-state double-face alignment with real codex behavior).
- First-user admin bootstrap: the first account to register on a fresh database automatically becomes `platform_admin`; `ADMIN_TOKEN` is now optional.
- GC optimization batch: zero-allocation single-pass usage extraction and per-stream relay channel merging.
- GC tuning hooks (`GOGC`/`GOMEMLIMIT`) wired through compose and `.env.example`, with load-test numbers documented (off by default).

### Changed

- codex-sdk updated to latest master (HTTP metadata injection, WS turn-state verification, pre-filter short-circuit).
- Deployment layout: `compose.yml` moved to the repository root — plain `docker compose up` now works with root `.env` auto-load; `deploy/` holds the production config template and the data directory.

### Fixed

- Deterministic CI: PostgreSQL partition fixtures now explicitly pre-create the fixed-date partitions they write into; the transport pool-reuse test proves 16 in-use connections via a barrier-controlled upstream instead of inferring pool capacity from dial counts.
- Dependency security: nanoid (CVE) and js-yaml (CVE-2026-59870 `!!omap` ReDoS) upgraded via pnpm overrides.
- Load-test tooling: NOTIFY snapshot-window 401s on fresh user creation and USD pricing units for key fill.

## [v0.0.1-beta.1] - 2026-08-15

First public beta release.

### Breaking

- **No migration path.** Databases and configurations are **not backward-compatible** across versions. Upgrading requires a brand-new database and a re-checked configuration (fresh setup) — no upgrade or migration tooling is provided.

### Added

- AI gateway core: OpenAI Responses API (REST + WebSocket), Anthropic Messages API, OpenAI Chat Completions API, OpenAI Images API, Codex web search, and the OpenAI-compatible model list behind a single entry point, with model routing, quotas, usage accounting, and a rules engine (routing, rate limiting, 429/error backoff).
- Embedded admin console (React, `/admin`) with a full OpenAPI-defined admin API: template/account management, per-user balance and FEFO temp quotas, usage statistics and billing, pricing sync from litellm.
- SDK integration: auth lifecycle (credential rotation with expiry preservation, failure recovery), WebSocket business-frame handling, and SDK-grade HTTP client connection pooling.
- Real-PostgreSQL test base for repository/integration tests (dedicated test database; tests skip when `TEST_DATABASE_URL` is unset).

### Changed

- Ops convergence: config keys renamed for semantics (`stats_flush_interval` → `quota_flush_interval`), fail-fast startup validation (unknown keys and placeholder secrets rejected), billing enabled by default.
- Usage statistics moved to an offline aggregation worker; database tables are created fresh — legacy "align-patch" compatibility was removed (all tables are new).

### Fixed

- Load-test storm fixes: partition drift (fresh-database policy), oversized batched flushes, missing event-name frames in streaming conversion, rejection storms, and shutdown truncation of in-flight batches.
- resp-ws 501 (HTTP Hijacker forwarding), SDK HTTP-client connection storms, and the `clientFor` race.
- Stats/overview display-caliber fixes (USD unit, TTFT metrics with histogram interpolation, compact number formatting).
