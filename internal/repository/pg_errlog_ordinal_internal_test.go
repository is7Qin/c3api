// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 自举路径的 err_logs 形状锚（S2b）：`EnsureErrLogPartitioned` 从零建出来的
// err_logs 必须是**0003 之后的当前形状**，不是 0001 的形状。
//
// 为什么需要单独一条：迁移建出来的表天然有 attempt_ordinal（0003 的 ALTER），所以
// 所有基于 pgtest.Clone 的 PG 用例都看不见这个缺陷；只有「ent migrate（排除
// err_logs）+ 自举建表」这条路径会漏掉那一列，而 ent 的读查询 SELECT 全列
// （含 attempt_ordinal）⇒ 首个读请求就是 42703。这条用例就是那个真实故障的
// 最小复现（评审要求的"自举路径必须产出当前形状"）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestPGInternalErrLogBootstrapHasAttemptOrdinal(t *testing.T) {
	repos, pool := g0InternalDB(t) // 空库 + ent migrate（err_logs 被排除）+ 自举建表
	ctx := context.Background()

	var dataType, nullable string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT data_type, is_nullable FROM information_schema.columns
		 WHERE table_name = 'err_logs' AND column_name = 'attempt_ordinal'`).Scan(&dataType, &nullable),
		"自举建出来的 err_logs 必须带 attempt_ordinal")
	require.Equal(t, "smallint", dataType)
	require.Equal(t, "YES", nullable, "循环不可见的行没有序号：必须可空")

	// 写入面真的能写它，且读得回来（ent 的 SELECT 是全列——缺列即 42703）。
	ordinal := int16(7)
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{{
		RequestID: "bootstrap-ordinal", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
		ErrorType: domain.Err5xx, CreatedAt: g0ProbeSeedTime.Add(time.Minute),
		AttemptOrdinal: &ordinal,
	}}))
	// 读回走 SQL（与 S3 的读法一致），不依赖 ent 的列清单。
	var got int16
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT attempt_ordinal FROM err_logs WHERE request_id = $1`, "bootstrap-ordinal").Scan(&got))
	require.EqualValues(t, 7, got)

	// 反向：nil ⇒ NULL（"没有 attempt 语义"不能变成 0）。
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{{
		RequestID: "bootstrap-null", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
		ErrorType: domain.Err4xx, CreatedAt: g0ProbeSeedTime.Add(time.Minute),
	}}))
	var isNull bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT attempt_ordinal IS NULL FROM err_logs WHERE request_id = $1`, "bootstrap-null").Scan(&isNull))
	require.True(t, isNull, "nil = 未知 ⇒ NULL，不是 0")
}
