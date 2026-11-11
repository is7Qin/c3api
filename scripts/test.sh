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

# Gate: no t.Parallel() calls (AGENTS.md). Runs on Linux (CI and dev hosts),
# where grep is always present. The parens are matched as [(][)] but the match
# is still plain text, so a *_test.go comment must not quote the call either —
# prose says "不并行" and cites AGENTS.md instead.
if grep -rEn --include='*_test.go' 't\.Parallel[(][)]' .; then
	echo "scripts/test.sh: t.Parallel() is forbidden (AGENTS.md)" >&2
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
#
# TestBenchGates asserts ns/op against the M4 record (×2.5). That number only
# means something on an idle machine: measured together with the rest of the
# suite (-p parallel) it read 2.7× once, while the same binary run alone reads
# 0.9–1.0× — load noise, not a regression. So the parallel pass skips it and it
# is re-run on its own below (single package, single test).
go test -p "$p" -timeout 30m -skip '^TestBenchGates$' "$@" ./...
go test -p 1 -timeout 30m "$@" -run '^TestBenchGates$' ./internal/protoconv
