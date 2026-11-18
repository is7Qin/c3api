// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate/gen"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// R3-Tc：-check 的退出码是 CI 的唯一信号。run 必须把 gen.Check 的错误映射成
// 非零退出码、把漂移文件写进 stderr——否则「删掉 os.Exit(1)」这种变异全绿。
func TestRunCheckExitCode(t *testing.T) {
	dsn := pgtest.DSN(t)
	dir := t.TempDir()
	migrations := filepath.Join(dir, "migrations")
	testdata := filepath.Join(dir, "testdata")
	cfg := gen.Config{DSN: dsn, MigrationsDir: migrations, TestdataDir: testdata}
	artifacts, err := gen.Run(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, artifacts.Write(cfg))

	args := []string{
		"-check",
		"-dsn", dsn,
		"-migrations", migrations,
		"-testdata", testdata,
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, run(args, &stdout, &stderr), "an in-sync tree must exit 0; stderr: %s", stderr.String())
	require.Contains(t, stdout.String(), "match")
	require.Empty(t, stderr.String())

	tampered := filepath.Join(migrations, "0001_baseline.up.sql")
	body, err := os.ReadFile(tampered)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tampered, append([]byte("-- drift\n"), body...), 0o644))

	stdout.Reset()
	stderr.Reset()
	code := run(args, &stdout, &stderr)
	require.NotZero(t, code, "a drifted tree must exit non-zero")
	require.Contains(t, stderr.String(), "0001_baseline.up.sql", "stderr must name the drifted file")
	require.Contains(t, stderr.String(), "stale")
	require.NotContains(t, stdout.String(), "match", "a failed -check must not claim success")
}

// TestMainIsOnlyTheExitWrapper 用源码级断言钉住 main 的形状：它必须恰好是
// os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))，一行都不多。TestRunCheckExitCode
// 只能证明「run 被调用时」的退出码，证明不了 main 真的把退出码交给 os.Exit——
// 删掉 os.Exit（或往 main 里再塞逻辑）时本用例直接红。
func TestMainIsOnlyTheExitWrapper(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must locate this test file")
	path := filepath.Join(filepath.Dir(file), "main.go")
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err)

	var mainFn *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, isFunc := decl.(*ast.FuncDecl); isFunc && fn.Recv == nil && fn.Name.Name == "main" {
			mainFn = fn
			break
		}
	}
	require.NotNil(t, mainFn, "main must exist")
	require.Len(t, mainFn.Body.List, 1, "main must contain exactly one statement")

	var printed bytes.Buffer
	require.NoError(t, printer.Fprint(&printed, fset, mainFn.Body.List[0]))
	require.Equal(t, "os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))", printed.String(),
		"main must be a pure exit-code wrapper; keep run(args, stdout, stderr) testable")
}
