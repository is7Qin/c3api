#!/usr/bin/env bash
set -euo pipefail

# The single-database test entrypoint.
#
# Every package's PostgreSQL integration tests run against their own private
# database, cloned from one migrated template (CREATE DATABASE ... TEMPLATE).
# Tests therefore no longer share a schema, so package test binaries may run
# concurrently: the old -p 1 serialisation is gone. Parallelism is injected
# explicitly (see below) so it stays bounded.
#
# TEST_DATABASE_URL is inherited from the environment and reaches every test
# binary unchanged; its role needs CREATEDB (or superuser), checked up front.
# C3API_TEST_MAINT_DB overrides the maintenance database (default: postgres);
# C3API_TEST_TEMPLATE overrides the template name; C3API_TEST_P overrides the
# default parallelism.
#
# It also gates a repo discipline that keeps the suite deterministic: no
# t.Parallel() anywhere in the test tree (AGENTS.md).
#
# Usage: TEST_DATABASE_URL=postgres://... scripts/test.sh [extra go test flags]

# Gate: no t.Parallel() calls. Comment lines are excluded (the repo has a few
# comments that mention the literal t.Parallel()). It uses git grep — not
# ripgrep — because this script runs on Linux (CI and dev hosts), where git is
# always present, so the gate needs no tool the environment does not ship.
parallel_hits="$(git grep -n -E -e 't\.Parallel\(\)' --and --not \( -e '^[[:space:]]*//' -o -e '^[[:space:]]*/\*' \) -- '*_test.go' || true)"
if [ -n "$parallel_hits" ]; then
	echo "scripts/test.sh: t.Parallel() is forbidden (AGENTS.md); found:" >&2
	echo "$parallel_hits" >&2
	exit 1
fi

# Parallelism: min(GOMAXPROCS, 8) by default, C3API_TEST_P overrides it, and a
# -p in "$@" wins because it is passed to go test after ours (Go "last wins").
default_p="$(go run ./internal/testsupport/pgtestctl p)"
p="${C3API_TEST_P:-$default_p}"

if [ -n "${TEST_DATABASE_URL:-}" ]; then
	# Fail fast if the role cannot create the template and per-test clones, and
	# drop template databases left behind by earlier schema sources.
	go run ./internal/testsupport/pgtestctl precheck
	go run ./internal/testsupport/pgtestctl sweep
fi
# A per-package timeout well above go test's 10m default: cloning a database is
# cheap on Linux CI but a few seconds each on slower hosts, and the repository
# package alone opens a clone per test. A -timeout in "$@" still wins.
exec go test -p "$p" -timeout 30m "$@" ./...
