# Contributing to c3api

Thanks for considering a contribution! This project follows an **issue-first workflow**: open a GitHub issue to discuss bugs and features **before** writing code — especially for features, discuss the design first.

## Development environment

- **Go 1.26** (see `go.mod`).
- **Node.js + pnpm** for the admin console (`web/`). On Windows, install dependencies with `pnpm install --config.node-linker=hoisted` (this machine's junction limitation); on Linux/CI the standard linker is fine.
- **Real PostgreSQL 18** for tests that exercise the repository layer. Start the dedicated test database once:

  ```bash
  docker compose -f deploy/test-compose.yml up -d
  export TEST_DATABASE_URL='postgres://postgres:c3api@localhost:15432/c3api_test'
  # ... run tests ...
  docker compose -f deploy/test-compose.yml down
  ```

  The test compose is isolated from the production stack (port 15432, dedicated database, no data volume).

## Testing discipline (required)

- **Repository-layer changes must be verified against the real PostgreSQL test base.** Tests that need a database are skipped when `TEST_DATABASE_URL` is unset (`t.Skip`) — so a green local run without it proves nothing for those tests. CI runs the full suite with the real database. Service/handler-layer pure unit tests follow the per-package convention.
- **Contract changes must be regenerated.** If you touch `openapi/openapi.yaml`, re-run both generators and commit the generated output:

  ```bash
  go generate ./internal/handler/
  pnpm gen:api
  ```

  CI fails when the generated code is out of sync with the contract.

## Code discipline

Before changing code, read the discipline checklist in [docs/architecture-topology.md](./docs/architecture-topology.md) §15 and go through it item by item.

## License and CLA

c3api is dual-licensed (AGPL-3.0-or-later + commercial). External code contributions require a CLA so they can be merged under this scheme:

- Read [CLA.md](./CLA.md) for the contribution terms.
- Signing is **automated** by the CLA Assistant workflow (`.github/workflows/cla.yml`): on your first pull request the bot will comment with the sign phrase — reply with that phrase in a comment to sign. You sign **once**, and it applies to all your future contributions. Signatures are recorded on the `cla-signatures` branch.

## Pull request process

- Keep PRs **small** and focused on a single issue.
- **Tests complete**: run the affected package suites (repository-layer changes against the real PostgreSQL base, with `-race`).
- **Contract generation up to date** when the API changed (see above).
- Commit messages in Chinese or English are both fine — match the repository's existing style (`type(scope): description`).
- The CLA status check gates the merge: unsigned PRs cannot be merged.
