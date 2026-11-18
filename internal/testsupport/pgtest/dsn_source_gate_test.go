// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package pgtest_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file is the structural gate behind "the require-PG switch must fail, never
// skip, when the PG test DSN is absent". A *_test.go may not spell the DSN env
// name at all — not in code, not in comments, not in error strings — and code may
// not reconstruct it by concatenation either. The text-level half mirrors the
// repo's no-parallel-tests discipline; the small string-constant fold is needed
// because "TEST_" + "DATABASE_URL" never contains the contiguous name in source.
//
// The name is built by concatenation so this gate does not flag itself; the gate
// file is also explicitly allowlisted below.
var (
	dsnEnvName    = "TEST_" + "DATABASE_URL"
	requirePGName = "TEST_" + "REQUIRE_PG"
)

// gateSelfPath has to rebuild the env name it forbids, which the fold half of
// this very test would flag; exempting it is the only way to state the rule
// without lying about the source text.
const gateSelfPath = "internal/testsupport/pgtest/dsn_source_gate_test.go"

// e2eDSNAllowlist is the only exemption for naming the DSN env: manual
// end-to-end tests. They carry a //go:build e2e constraint (so `go test ./...`
// never compiles them) and fall back to a hard-coded DSN to hit a real e2e
// database when the env var is absent — the unit-test gate does not apply.
// Adding an e2e file means adding it here deliberately; there is no name-based
// wildcard.
var e2eDSNAllowlist = map[string]string{
	"tools/e2e/billing_e2e_test.go":           "manual e2e behind //go:build e2e",
	"tools/e2e/routing_e2e_test.go":           "manual e2e behind //go:build e2e",
	"tools/e2e/routing_multi_e2e_test.go":     "manual e2e behind //go:build e2e",
	"tools/e2e/supplier_earnings_e2e_test.go": "manual e2e behind //go:build e2e",
}

// requirePGAllowlist is the only place outside internal/testsupport/pgtest that
// may spell the gate's env name: internal/config's namespace regression test
// has to set exactly this variable to prove Load() does not parse it as a
// config key (the C3API_ prefix trap).
var requirePGAllowlist = map[string]string{
	"internal/config/config_test.go": "namespace regression test must set the exact env",
}

// TestPGTestsReadDSNOnlyThroughPgtest fails for every *_test.go that names the
// DSN env in its text (code or comments) or reconstructs the name from string
// constants.
func TestPGTestsReadDSNOnlyThroughPgtest(t *testing.T) {
	root := repoRoot(t)
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		rel := repoRel(t, root, path)
		if entry.IsDir() {
			if skipDir(rel, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		text := string(body)

		if rel == gateSelfPath {
			return nil // the gate has to rebuild the name it forbids
		}
		if reason, allowed := e2eDSNAllowlist[rel]; allowed {
			require.True(t, hasE2EBuildTag(text),
				"%s is allowlisted (%s) but has no //go:build e2e constraint; remove it from the allowlist", rel, reason)
			return nil
		}

		lines := lineHits(text, dsnEnvName)
		lines = append(lines, foldedEnvNameLines(t, rel, body)...)
		slices.Sort(lines)
		lines = slices.Compact(lines)
		for _, line := range lines {
			violations = append(violations, fmt.Sprintf(
				"%s:%d names %s; use pgtest.DSN(t)/CloneEmpty(t) so the require-PG switch is honored",
				rel, line, dsnEnvName))
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, violations, "test files must not name or reconstruct the DSN env:\n%s", strings.Join(violations, "\n"))
}

// foldedEnvNameLines parses the file and flags any string expression (literal or
// `+` chain) whose compile-time value contains the env name. This is what catches
// "TEST_" + "DATABASE_URL" and a const used indirectly.
func foldedEnvNameLines(t *testing.T, rel string, body []byte) []int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, body, parser.SkipObjectResolution)
	require.NoError(t, err, "parse %s", rel)
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		value, ok := foldString(expr)
		if !ok || !strings.Contains(value, dsnEnvName) {
			return true
		}
		lines = append(lines, fset.Position(expr.Pos()).Line)
		return true
	})
	return lines
}

// foldString evaluates the subset of string expressions that can reconstruct an
// env name: literals, parentheses and `+` concatenation.
func foldString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return value, true
	case *ast.ParenExpr:
		return foldString(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, leftOK := foldString(e.X)
		right, rightOK := foldString(e.Y)
		if !leftOK || !rightOK {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// TestRequirePGEnvLivesOnlyInPgtest keeps the gate single-sourced: a new
// TEST_REQUIRE_PG check in another package is a second truth by definition.
func TestRequirePGEnvLivesOnlyInPgtest(t *testing.T) {
	root := repoRoot(t)
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		rel := repoRel(t, root, path)
		if entry.IsDir() {
			if skipDir(rel, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		if strings.HasPrefix(rel, "internal/testsupport/pgtest/") {
			return nil // the gate package owns the env name
		}
		if _, allowed := requirePGAllowlist[rel]; allowed {
			return nil
		}
		body, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		for _, line := range lineHits(string(body), requirePGName) {
			violations = append(violations, fmt.Sprintf(
				"%s:%d mentions %s outside internal/testsupport/pgtest; use pgtest.DSN instead of a second gate",
				rel, line, requirePGName))
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, violations, "TEST_REQUIRE_PG must have a single owner:\n%s", strings.Join(violations, "\n"))
}

// repoRoot walks up from this file until go.mod is found. No cwd guessing.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must locate this test file")
	dir := filepath.Dir(file)
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found above the pgtest package")
		dir = parent
	}
}

// repoRel returns the slash-separated path relative to root.
func repoRel(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	require.NoError(t, err)
	return filepath.ToSlash(rel)
}

// skipDir prunes non-source trees (VCS, dependencies, generated assets, plans).
func skipDir(rel, name string) bool {
	switch name {
	case ".git", ".omo", "_scratch", "node_modules", "vendor":
		return true
	}
	return rel == "web/dist" || strings.HasPrefix(rel, "web/dist/")
}

// lineHits returns 1-based line numbers containing any of patterns.
func lineHits(text string, patterns ...string) []int {
	var hits []int
	for i, line := range strings.Split(text, "\n") {
		for _, pattern := range patterns {
			if strings.Contains(line, pattern) {
				hits = append(hits, i+1)
				break
			}
		}
	}
	return hits
}

// hasE2EBuildTag reports whether the file starts (comments/blank lines only)
// with a //go:build e2e constraint.
func hasE2EBuildTag(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			if trimmed == "//go:build e2e" {
				return true
			}
			continue
		}
		return false // reached the package clause or code
	}
	return false
}
