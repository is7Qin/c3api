# Changelog

All notable changes to c3api are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versioning follows the policy below.

## Versioning

During the **beta** phase, versions are `v0.x.0-beta.N` (N increments with each release). The first beta is `v0.0.1-beta.1`; concrete numbers for later releases are decided at tag time.

## [Unreleased]

### Added

- Email service: registration email verification codes and password reset by emailed code — SMTP relay configured through runtime settings (`mail.*`, admin console → Settings → Mail tab, disabled by default), editable Chinese-default email templates with built-in fallback, async-safe sync send with 15 s timeout.
- Admin settings page reorganized into category tabs (signup / defaults / pricing sync / tier policy / cluster / mail), with a Mail tab covering SMTP config and template editors; new user-facing pages for register code entry and forgot-password flow.

## [v0.0.1-beta.4] - 2026-08-22

### Breaking

- All management/user APIs moved under the `/api` prefix (`/api/admin/*`, `/api/user/*`); the SPA is now served from the repository root. The data plane (`/v1/*`) is unchanged — scripts and integrations must update their base URLs.

### Added

- Rules engine composite conditions: `http_status_in` / `model_in` / `error_message_contains_in` OR-match sets (mutually exclusive with the single-value forms, empty arrays rejected with 400), precompiled into zero-allocation lookup sets on the hot path; a fully-empty `then {}` is now a valid pure-passthrough rule.
- Rules-driven response shaping: `response_code`/`custom_message` are pointer-intent (nil passes the upstream code/message through), the event taxonomy is `ok/429/4xx/5xx/network`, unmatched non-ok errors normalize to a fixed 502 message, user-face error logs are sanitized accordingly, and `when.model` matches the final mapped model uniformly across the REST/WS/log surfaces.
- Batch cooldown reset: new `POST /accounts/batch-reset-cooldown`; writing `status=active` through PUT/batch-update also clears the cooldown — manually recovered accounts become selectable immediately on every instance.
- Codex credential batch import (OAuth/PAT); PAT revocation detection unified inside codex-sdk (AT-401 classifier owns the death codeset, wired to the fatal-disable callback), and WS death-frame classification delegates to the same source.
- Per-account usage snapshots surfaced to admins (TTL-cached, bounded-concurrency fetch) with a batch aggregation endpoint; `raw_cost` flows end-to-end (usage logs → offline stats aggregation → API → UI).
- Template model hard whitelist: non-matching models return 404; tier-2 selection narrows to full-model accounts.
- Admin console: glass design-system refresh with Apple palette, codex multi-format import dialog (CPA/folder), account usage detail dialog, cooldown badge with remaining time, client IP columns on all four log tables, composite-IN condition editors, custom 404 page.
- Contributor knowledge base: hierarchical `AGENTS.md` (root / internal / web).

### Changed

- codex-sdk updated to `aef6a68`: exported auth-frame classifier and SDK-owned PAT death judgment (the gateway-side 401 heuristic was removed).
- Snapshot rebuilds reuse previous instances so concurrency/reuse counters stay continuous; static views swap atomically; the scheduler's in-memory status/cooldown is the authoritative source shown in the account list.
- OpenAPI: the rules `when`/`then` schema reference is fully documented (kind values, model semantics, empty-array rejection, pure-passthrough meaning).
- README gained a performance section (pprof CPU profile findings and measured GC-tuning numbers).

### Fixed

- Freshly created users could hit a 401 on their first immediate request (the debounced JWT-snapshot reload lagged the creation) — user create/register/update now update the local snapshot instantly.
- Rule cooldown defects: cooldown-only punishments were silently dropped, ok-recovery could fire while a cooldown was still unexpired, and rebuilds lost in-memory cooldowns when the DB column was NULL; the `disabled` action now persists to the database.
- Codex dial 4xx responses bypassed rule-engine punishment — they are now classified and punished like every other failure surface.
- Protocol conversion fallback missed `ErrNoAvailable`, and group-level protocol-convert edits did not propagate to key metadata (now registered incrementally with the Keys NOTIFY bit).
- Web: a dead import broke the production build (`tsc`), the user-profile page lacked its breadcrumb, the bucket time column truncated by granularity, the mobile menu lost its dropdown context, and several table-styling regressions.

## [v0.0.1-beta.3] - 2026-08-17

### Added

- Client IP capture in usage and error logs (`client_ip` column on both tables, exposed via the admin/user log APIs): `proxy.behind_cdn` (default false) enables vendor header detection — `CF-Connecting-IP` → `True-Client-IP` → `X-Real-IP` in order, `RemoteAddr` fallback; off = `RemoteAddr` only. Zero-allocation extraction on the request path.
- Pagination (`limit`/`offset`) for the redemption-code usage audit endpoint — previously silently truncated at 20 rows.
- Masked admin key listing (`GET /admin/keys`) with name/user/group filters and pagination.
- WS upgrade handshake timeout (15 s) — a black-hole upstream can no longer pin concurrency slots forever.
- Service-side search for the logs filters and a unified ScrollArea scrollbar across the admin UI.

### Changed

- Forwarding pipeline unified into a single shared skeleton: `handleFormat`/`HandleSearch`/`HandleResponsesWS` now share one guard stage (auth → quota → balance → concurrency gate → rate limit) and one failover loop, with per-format differences confined to two narrow interfaces (attempt + sink). Warn wording, per-format prechecks and the WS exhaustion frame stay format-specific.
- WS relay (responses-ws and codex variants) unified onto a 5-method transport interface — the two 200-line concurrent state machines are now one skeleton plus thin adapters.
- Key/group lifecycle semantics hardened: soft-deleted keys can no longer be resurrected through update/rotate (404), soft-deleted groups reject key creation and assignment (404), group deletion validates account membership (409, including the batch path), key updates are patch-based (only provided fields are written), and assignment replacement is transactional.
- Admin role revocation takes effect immediately: the admin auth path now trusts the snapshot role (fail-closed when the snapshot is missing) instead of the 24 h JWT claim.
- WS relay goroutines are panic-contained per connection (log + orderly teardown) instead of crashing the whole process; the panic log now includes a stack trace and no longer writes a 500 body into an already-started SSE stream.
- Admin/user list endpoints clamp `limit` to 200; strict JSON decoding rejects unknown fields and trailing garbage with a 400.
- Config validation is fail-fast for `proxy.max_body_size` (≥ 1) and `upstream.idle_conn_timeout`/`dial_timeout` (≥ 1 ms).
- User-visible error frames never carry SDK/connection internals: codex fatal/4xx paths use fixed gateway messages, the aiclient 4xx fallback keeps internal text in logs only, and images SSE error frames use a fixed message.
- JSON processing family converged: single-pass top-level extraction (4 full-document scans → 2), byte-level sjson rewrites (preserving >2^53 integer precision), byte-anchored event-type detection, and a usage pre-filter on the chat path — all pinned by zero-allocation assertions.
- Web frontend: poll failures keep stale data with a warning bar instead of replacing the whole page; log filter inputs are debounced (300 ms); settings render unknown keys in a fallback card.

### Fixed

- SSE long-line truncation zeroing billing/quota on long responses (line-continuation state machine).
- WS gateway-credential passthrough: `X-Api-Key` is now stripped from the upstream handshake like `Authorization`.
- WS path ignoring `service_tier` — tier extraction, strip/reject policy and billing tier are now applied on the WS first frame.
- Converted-path `tt = 0` never deducting quota — all three exit points mirror the native `tt = it + ot`.
- `failover_attempts = 0` leaking concurrency slots permanently (validated ≥ 1, plus a defensive release on the exhaust path).
- Scheduler resurrection race: `apply`/`FailAccount` are now copy-on-write CAS with a disabled absorbing state.
- Anthropic SDK validation errors classified as 4xx instead of network errors.
- Test flakes: stats-agg first-round assertion timing, and the user-key lifecycle list assertion depending on fake-store map iteration order.

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
