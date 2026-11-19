// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 隔离门（spec 2026-11-18 retry-observability §3.2 末条 / §6 的 S2 验收 7）：
// `attempt_logs` 只写不读——统计读面（stat_repo / stat_query_repo / stat_entity_agg
// 与 usage 包的聚合 worker）一旦开始读它，口径立刻失真：这张表的行是「一次失败
// dispatch」，不是「一次请求」，读它会把重试次数算进请求数。
//
// 做成 Go 测试而不是 CI 里的 grep：它跟包一起跑、失败信息带文件名，且不依赖
// shell 工具（Windows 上 `rg` 可用性不是契约）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// statsReadSurface 是统计读/写面的生产文件 glob（相对本包目录；测试文件不参与
// ——它们可以断言"这里不许出现 attempt_logs"）。
//
// 与 spec §3.2 的 `rg "attempt_logs" internal/repository/stat_* internal/usage/stats*`
// 相比收窄了第二段：`internal/usage/stats.go` 是 **/ops/workers 的投影结构体**
// （S2a 在那里加了 attempt_logs 的保留期字段），它不持有任何查询——把"文本不出现"
// 当契约会让这两个正当字段永远红。真正会读表的是 `stats_agg*.go`（聚合 worker
// 发 SQL 的地方）与 `stat_*.go`（六条查询），所以门只钉这两处。
var statsReadSurface = []string{
	"stat_*.go",
	filepath.Join("..", "usage", "stats_agg*.go"),
}

func TestAttemptLogsUnreadByStats(t *testing.T) {
	var scanned []string
	var offenders []string
	for _, pattern := range statsReadSurface {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			scanned = append(scanned, path)
			for i, line := range strings.Split(string(body), "\n") {
				if strings.Contains(line, "attempt_logs") {
					offenders = append(offenders, fmt.Sprintf("%s:%d", path, i+1))
				}
			}
		}
	}
	require.NotEmpty(t, scanned, "扫描面为空：glob 写错了？统计读面必须真的被检查")
	require.Contains(t, scanned, filepath.Join("..", "usage", "stats_agg.go"),
		"扫描面必须覆盖 usage 聚合 worker（漏了它这条门只保护一半）")
	require.Empty(t, offenders,
		"attempt_logs 只写不读（观测面不是账）：统计读面出现引用的位置 %v", offenders)
}
