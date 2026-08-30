// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 时间/价格快照五列（ttft_ms + 4×price_*_millis）真实 PG 测试：bootstrap 建表
// 含 5 列（bigint，可空）；有值/NULL（未计费路径语义）roundtrip（QueryLogs 读回）
// + SQL 层直查确认 NULL 语义。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 +
// migrate（钩子跳过 usagelog）+ 分区 bootstrap（DDL 含 5 列）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// pgLogSnapshotCols 五列元数据（data_type/is_nullable 断言用）。
var pgLogSnapshotCols = []struct{ name, dataType string }{
	{"ttft_ms", "bigint"},
	{"price_input_millis", "bigint"},
	{"price_output_millis", "bigint"},
	{"price_cache_read_millis", "bigint"},
	{"price_cache_creation_millis", "bigint"},
}

func TestUsageLogSnapshotColumnsRoundtripPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	pool := pgSharedPool(t)

	// bootstrap 建表必须含 5 列（bigint，无默认值 = NULL 可空）
	for _, c := range pgLogSnapshotCols {
		var dataType, isNullable string
		err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = $1`, c.name).
			Scan(&dataType, &isNullable)
		require.NoError(t, err, "bootstrap 建表必须含 %s 列", c.name)
		require.Equal(t, c.dataType, dataType)
		require.Equal(t, "YES", isNullable)
	}

	// 有值 roundtrip：5 列全填（价格 = 每 M token 毫分，pricing 同款单位）
	l1 := usageLogFor("snap-1", time.Now().UTC())
	l1.TTFTMS = int64Ptr(88)
	l1.PriceInputMillis = int64Ptr(1e7)  // $100 / 1M
	l1.PriceOutputMillis = int64Ptr(2e7) // $200 / 1M
	l1.PriceCacheReadMillis = int64Ptr(1234)
	l1.PriceCacheCreationMillis = int64Ptr(5678)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l1}))

	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	require.NotNil(t, got.TTFTMS, "ttft_ms 有值必须读回")
	require.Equal(t, int64(88), *got.TTFTMS)
	require.Equal(t, int64(1e7), *got.PriceInputMillis)
	require.Equal(t, int64(2e7), *got.PriceOutputMillis)
	require.Equal(t, int64(1234), *got.PriceCacheReadMillis)
	require.Equal(t, int64(5678), *got.PriceCacheCreationMillis)

	// SQL 层直查确认落库值（不经 domain 映射）
	var rawTTFT, rawIn, rawOut, rawCR, rawCC int64
	err = pool.QueryRow(ctx, `SELECT ttft_ms, price_input_millis, price_output_millis,
		price_cache_read_millis, price_cache_creation_millis
		FROM usage_logs WHERE request_id = 'snap-1'`).
		Scan(&rawTTFT, &rawIn, &rawOut, &rawCR, &rawCC)
	require.NoError(t, err)
	require.Equal(t, int64(88), rawTTFT)
	require.Equal(t, int64(1e7), rawIn)
	require.Equal(t, int64(2e7), rawOut)
	require.Equal(t, int64(1234), rawCR)
	require.Equal(t, int64(5678), rawCC)

	// NULL 语义（未计费路径/无首 token）：5 列全不设置 → 落库 NULL、读回 nil
	l2 := usageLogFor("snap-2", time.Now().UTC())
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l2}))

	rows, err = repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		if r.RequestID != "snap-2" {
			continue
		}
		require.Nil(t, r.TTFTMS, "未设置 ttft_ms → nil")
		require.Nil(t, r.PriceInputMillis, "未设置 price_input_millis → nil")
		require.Nil(t, r.PriceOutputMillis, "未设置 price_output_millis → nil")
		require.Nil(t, r.PriceCacheReadMillis, "未设置 price_cache_read_millis → nil")
		require.Nil(t, r.PriceCacheCreationMillis, "未设置 price_cache_creation_millis → nil")
	}

	// DB 层直查：未设置路径 5 列全为 NULL
	for _, c := range pgLogSnapshotCols {
		var raw *int64
		err = pool.QueryRow(ctx, `SELECT `+c.name+` FROM usage_logs WHERE request_id = 'snap-2'`).Scan(&raw)
		require.NoError(t, err)
		require.Nil(t, raw, "DB 层 %s 为 NULL", c.name)
	}
}
