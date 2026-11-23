// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// fixture_partition_span_pg_test.go 钉住「夹具回看跨 UTC 零点」这一根因本身，
// 不依赖运行时刻碰运气。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestWritePartitionsCoverPreviousUTCDayPG 钉住 ensureWritePartitions 的跨日契约：
// 夹具按「昨天 23:30」造行，而 openAttemptChainDB 只建 [今天, 明天] 的分区。
//
// 判据（本用例存在的唯一理由）：删掉下面的 ensureWritePartitions 调用，本用例在
// **任何时刻**都因 SQLSTATE 23514（no partition of relation ... found for row）
// 失败——锚点恒在前一个 UTC 日，与运行时刻无关；加回该行则通过。因此不要把它
// 弱化成「ensure 调用不报错」：三张表都真写真读回，证明分区确实接住了这些行，
// 而不是只建了空表。
func TestWritePartitionsCoverPreviousUTCDayPG(t *testing.T) {
	ctx := context.Background()
	repos, pool := openAttemptChainDB(t)

	// 无论当前几点，昨天 23:30 都落在前一个 UTC 日；+1m 仍同日，窗口天然跨日。
	at := time.Now().UTC().Truncate(24 * time.Hour).Add(-30 * time.Minute)
	from, to := at, at.Add(time.Minute)

	// 历史行是夹具的一部分：谁造谁建（见 ensureWritePartitions 注释）。
	ensureWritePartitions(t, ctx, repos, at)

	const (
		attemptReq = "partition-span-attempt"
		errReq     = "partition-span-err"
		usageReq   = "partition-span-usage"
	)
	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{{
		RequestID: attemptReq, Ordinal: 1, AccountID: 1, Lane: "primary",
		Format: "openai-chat", StatusCode: 502, Outcome: "5xx", ErrorType: "5xx",
		LatencyMS: 12, CreatedAt: at,
	}}))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{{
		RequestID: errReq, Model: "m", Format: domain.FormatOpenAIChat,
		StatusCode: 502, ErrorType: domain.Err5xx, LatencyMS: 12, CreatedAt: at,
	}}))
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{{
		RequestID: usageReq, Model: "m", Format: domain.FormatOpenAIChat,
		ErrorType: domain.ErrNone, TotalTokens: 1, Cost: 1, RawCost: 1, CreatedAt: at,
	}}))

	// 半开窗口 [from, to) 读回：行必须落在刚建好的昨天分区里，且请求号对得上。
	for _, tc := range []struct {
		table     string
		requestID string
	}{
		{"attempt_logs", attemptReq},
		{"err_logs", errReq},
		{"usage_logs", usageReq},
	} {
		t.Run(tc.table, func(t *testing.T) {
			var n int
			require.NoError(t, pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM `+tc.table+` WHERE request_id=$1 AND created_at >= $2 AND created_at < $3`,
				tc.requestID, from, to).Scan(&n))
			require.Equal(t, 1, n, "%s 必须恰有 1 行落在昨天 23:30 的分区里", tc.table)
		})
	}
}
