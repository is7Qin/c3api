// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoSDKBridgeImport pins the cleanup: service must
// not import internal/sdkbridge — the codex usage snapshot is called by the
// handler (ctor-injected prober), never by service.
func TestNoSDKBridgeImport(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", name)
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, "internal/sdkbridge") {
				found = true
			}
		}
		// 回填残留：字段/setter/接口名不得出现在任何非测试生产文件中。
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		for _, tok := range []string{"SetUsageSnapshotter", "usageSnapshots", "CodexUsageSnapshotter"} {
			require.NotContains(t, string(raw), tok, "%s 仍含回填残留 %s", name, tok)
		}
	}
	require.False(t, found, "service 生产代码不得 import internal/sdkbridge")
}
