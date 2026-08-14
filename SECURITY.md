# Security Policy

## Supported versions

c3api is currently in **beta**. There is a single rolling development line: security fixes land on `main` and are shipped with the next beta release. There are no long-term-support (LTS) versions during the beta phase.

## Reporting a vulnerability

If you find a security issue in c3api, **please do not open a public issue**.

- **Primary channel** — report privately via GitHub Security Advisory (private vulnerability reporting): <https://github.com/is7Qin/c3api/security/advisories/new>
- **Fallback** — if you cannot use the advisory form, reach the maintainer via GitHub: <https://github.com/is7Qin> (issue or Discussions, mentioning it is security-related).

When reporting, include if possible:

- the affected component and version;
- a minimal reproduction (request flow, relevant configuration);
- the impact you believe the issue has.

## Response commitments (beta)

- **Acknowledgement within 48 hours** of receiving a report.
- **High/critical severity: a fix published within 7 days** (a beta-phase commitment).

You will receive status updates as the fix progresses.

## Scope

The security scope is the c3api gateway itself:

- the Go binary (`cmd/`, `internal/`, `pkg/`);
- the embedded admin console frontend (built from `web/`, embedded via `go:embed`).

**Dependencies**: vulnerabilities in upstream dependencies are handled through the regular dependency update path (go.mod / web lockfile). If a dependency issue materially affects c3api, it is disclosed here.

## Fix process

1. The report is triaged and reproduced privately.
2. A fix is developed and tested privately (including the real-PostgreSQL test base).
3. The fix is released; the release note discloses the issue (with a CVE application if appropriate).

## Out of scope

- AGPL-3.0 obligations and compliance questions;
- commercial license inquiries — see [LICENSE.commercial](./LICENSE.commercial).
