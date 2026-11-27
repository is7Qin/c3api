// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoPostgresDriverImport 钉住本包的驱动边界：supplier 是**纯领域规则 + worker 编排**，
// 不得直接 import PostgreSQL 驱动。
//
// 为什么需要这条：allow 矩阵（internal/architecture）只裁决**仓库内部包**之间的边，
// 对 `jackc/pgx/*` 这类第三方驱动完全看不见。P4-1 把 `IsRetryableTxErr` 抽到
// `internal/pgretry` 之后，若没有这条门禁，"顺手"在本包再 import 一次 pgconn
// 不会有任何测试变红——那条抽取就会慢慢烂回去。
//
// 判据只看**非测试**源码（测试可以使用驱动构造 PgError 断言重试分类）。
func TestNoPostgresDriverImport(t *testing.T) {
	const forbidden = "github.com/jackc/pgx"
	files := []string{"aggregate.go", "credit.go", "freeze.go", "payout.go", "risk.go", "snapshot.go", "thaw.go", "view.go"}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", name)
		found++
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			require.NotContains(t, path, forbidden,
				"%s import 了 PostgreSQL 驱动 %q：驱动事实属 internal/pgretry（或调用方适配层），"+
					"本包必须保持与驱动无关，否则「纯领域规则」的边界失效", name, path)
		}
	}
	require.Equal(t, 8, found, "扫描的源文件数变了：新增/删除 supplier 文件时同步本清单")
}
