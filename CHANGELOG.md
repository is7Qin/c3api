# Changelog

All notable changes to c3api are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versioning follows the policy below.

## Versioning

During the **beta** phase, versions are `v0.x.0-beta.N` (N increments with each release). The concrete version number is decided when a release tag is created. The first beta is `v0.0.1-beta.1` (2026-08-15 user ruling).

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
