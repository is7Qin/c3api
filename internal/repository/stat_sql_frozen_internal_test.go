// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 统计聚合 SQL 冻结快照（spec 2026-11-18 retry-observability §6 的 S2 验收 8 次证）。
//
// LoadAggRange 是 usage_stats 重建的唯一读面（离线聚合 worker 每周期跑一次）。
// 本文件的字面量取自 `dbbbc2bd`（S2a 落地、attempt_logs 写路径提交之前）的实测
// 文本，逐字冻结：改统计口径必须同时改这里，并在提交说明里讲清楚口径变化——
// 「顺手改了一行 SQL」在本仓库是不允许悄悄发生的（它是账）。
//
// 三条红线：
//   - attempt_logs 是观测面，**不是账**：两源 SQL 都不得引用它；
//   - 0003 给 err_logs 加的 attempt_ordinal 不得进入统计投影（那会让「加列不改
//     统计」的承诺失效）；
//   - 文本逐字节相同（tab/换行都算），所以本测试对格式漂移也敏感。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	frozenAggUsageSQL = `SELECT date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       COALESCE(group_id, 0), COALESCE(model, ''),
	count(*), count(*) FILTER (WHERE error_type <> 'none'),
	sum(input_tokens), sum(output_tokens), sum(total_tokens),
	sum(cache_read_tokens), sum(cache_creation_tokens), sum(cost), COALESCE(sum(raw_cost), 0),
	sum(call_count),
	COALESCE(sum(ttft_ms), 0), count(ttft_ms), COALESCE(max(ttft_ms), 0), ARRAY[count(*) FILTER (WHERE ttft_ms < 50),
       count(*) FILTER (WHERE ttft_ms >= 50 AND ttft_ms < 100),
       count(*) FILTER (WHERE ttft_ms >= 100 AND ttft_ms < 200),
       count(*) FILTER (WHERE ttft_ms >= 200 AND ttft_ms < 400),
       count(*) FILTER (WHERE ttft_ms >= 400 AND ttft_ms < 800),
       count(*) FILTER (WHERE ttft_ms >= 800 AND ttft_ms < 1600),
       count(*) FILTER (WHERE ttft_ms >= 1600 AND ttft_ms < 3200),
       count(*) FILTER (WHERE ttft_ms >= 3200 AND ttft_ms < 6400),
       count(*) FILTER (WHERE ttft_ms >= 6400 AND ttft_ms < 12800),
       count(*) FILTER (WHERE ttft_ms >= 12800)]
FROM usage_logs WHERE created_at >= $1 AND created_at < $2 AND (error_type = 'none' OR COALESCE(commit_state, '') IN ('sent_ambiguous','response_started'))
GROUP BY 1, 2, 3`
	frozenAggErrLogSQL = `SELECT date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       COALESCE(group_id, 0), COALESCE(model, ''),
	count(*), count(*) FILTER (WHERE error_type <> 'none'),
	0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint,
	0::bigint, 0::bigint, 0::bigint, 0::bigint,
	ARRAY[0::bigint, 0, 0, 0, 0, 0, 0, 0, 0, 0]
FROM err_logs WHERE created_at >= $1 AND created_at < $2 AND (error_type <> 'none' AND COALESCE(commit_state, '') NOT IN ('sent_ambiguous','response_started'))
GROUP BY 1, 2, 3`
)

func TestStatAggSQLFrozen(t *testing.T) {
	require.Equal(t, frozenAggUsageSQL, aggUsageSQL, "usage_logs 聚合 SQL 必须逐字不变")
	require.Equal(t, frozenAggErrLogSQL, aggErrLogSQL, "err_logs 聚合 SQL 必须逐字不变")

	for name, sql := range map[string]string{"aggUsageSQL": aggUsageSQL, "aggErrLogSQL": aggErrLogSQL} {
		require.NotContains(t, sql, "attempt_logs", "%s 不得读取观测表 attempt_logs（统计只认 usage_logs/err_logs）", name)
		require.NotContains(t, sql, "attempt_ordinal", "%s 不得投影 0003 新列（加列不改口径）", name)
	}
}
