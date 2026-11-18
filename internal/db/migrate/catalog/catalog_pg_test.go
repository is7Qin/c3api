// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package catalog

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

func newEmptyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pgtest.CloneEmpty(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(context.Background()))
	return db
}

// TestDynamicRules 覆盖 spec §2.8 的三条动态规则：命名 YYYYMMDD、父归属、精确
// bounds。合格分区不产生违反项；粗区间/错名字/挂错父各产生一条。
func TestDynamicRules(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	for _, stmt := range []string{
		`CREATE TABLE public.probe_events (id bigint NOT NULL, created_at timestamptz NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE (created_at)`,
		`CREATE TABLE public.probe_events_20250101 PARTITION OF public.probe_events FOR VALUES FROM ('2025-01-01 00:00:00+00') TO ('2025-01-02 00:00:00+00')`,
		// 粗区间：两天合成一个分区。
		`CREATE TABLE public.probe_events_20250103 PARTITION OF public.probe_events FOR VALUES FROM ('2025-01-03 00:00:00+00') TO ('2025-01-05 00:00:00+00')`,
		// 命名不合规：没有 YYYYMMDD 后缀。
		`CREATE TABLE public.probe_events_jan PARTITION OF public.probe_events FOR VALUES FROM ('2025-01-06 00:00:00+00') TO ('2025-01-07 00:00:00+00')`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	m, err := Dump(ctx, db)
	require.NoError(t, err)
	require.Len(t, m.Dynamic.Partitions, 3)
	for _, p := range m.Dynamic.Partitions {
		switch p.Child {
		case "public.probe_events_20250101":
			require.True(t, p.Valid(), "a day-aligned partition must satisfy all three rules")
			require.Equal(t, "public.probe_events", p.Parent)
		}
	}
	joined := strings.Join(m.Dynamic.Violations, "\n")
	require.Contains(t, joined, "probe_events_20250103", "two-day bounds must be rejected")
	require.Contains(t, joined, "probe_events_jan", "non-YYYYMMDD names must be rejected")

	// 非 allowlist 父表的分区实例**不**被静默吞掉：它们出现在静态指纹里（漂移可见），
	// 这样漏登记 PartitionParents 会以红用例暴露，而不是让对象凭空消失。
	require.Contains(t, m.Static, "table|probe_events_20250101|")
	require.Contains(t, m.Static, "table|probe_events|relkind=p|")
}

// TestDateSuffixedBusinessTableStaysInManifest 是 R2-3 的定点用例：名字带日期的
// **普通业务表**不是分区实例，必须在静态指纹里（旧实现按名字后缀排除会把它吞掉）。
func TestDateSuffixedBusinessTableStaysInManifest(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := db.ExecContext(ctx, `CREATE TABLE public.foo_20240101 (id bigint NOT NULL, note varchar NOT NULL DEFAULT '', PRIMARY KEY (id))`)
	require.NoError(t, err)

	m, err := Dump(ctx, db)
	require.NoError(t, err)
	require.Contains(t, m.Static, "table|foo_20240101|relkind=r|")
	require.Contains(t, m.Static, "column|foo_20240101|attnum=1|name=id|")
	require.Contains(t, m.Static, "index|foo_20240101_pkey|table=foo_20240101|")
	require.Empty(t, m.Dynamic.Partitions, "a plain table is not a partition instance")
}

// TestAllowlistedParentPartitionsLeaveStaticManifest 锚定排除判定的正向：只有
// 已知父表的按日分区才走 Dynamic 视图、不进静态指纹。
func TestAllowlistedParentPartitionsLeaveStaticManifest(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	for _, stmt := range []string{
		`CREATE TABLE public.usage_logs (id bigint NOT NULL, created_at timestamptz NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE (created_at)`,
		`CREATE TABLE public.usage_logs_20240101 PARTITION OF public.usage_logs FOR VALUES FROM ('2024-01-01 00:00:00+00') TO ('2024-01-02 00:00:00+00')`,
		`CREATE TABLE public.foo_20240101 (id bigint NOT NULL, PRIMARY KEY (id))`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	m, err := Dump(ctx, db)
	require.NoError(t, err)
	require.Contains(t, m.Static, "table|usage_logs|relkind=p|", "the parent stays in the static manifest")
	require.NotContains(t, m.Static, "usage_logs_20240101", "allowlisted parent partitions stay out of the static manifest")
	require.Contains(t, m.Static, "table|foo_20240101|relkind=r|", "a date-suffixed business table stays in")
	require.Empty(t, m.Dynamic.Violations)
	require.Len(t, m.Dynamic.Partitions, 1)
	require.Equal(t, "public.usage_logs", m.Dynamic.Partitions[0].Parent)
}

// TestDumpIgnoresControlTable 锚定 allowlist：runner 账本表不属于任何迁移版本。
func TestDumpIgnoresControlTable(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	_, err := db.ExecContext(ctx, `CREATE TABLE public.keep_me (id bigint PRIMARY KEY)`)
	require.NoError(t, err)
	before, err := Dump(ctx, db)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `CREATE TABLE public.`+ControlTable+` (version bigint PRIMARY KEY)`)
	require.NoError(t, err)
	after, err := Dump(ctx, db)
	require.NoError(t, err)
	require.Equal(t, before.Static, after.Static, ControlTable+" must stay out of the static fingerprint")

	// 但 view/trigger/policy/function 必须被报成违反项（本仓当前必须为空）。
	_, err = db.ExecContext(ctx, `CREATE VIEW public.keep_me_view AS SELECT id FROM public.keep_me`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE FUNCTION public.keep_me_fn() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
	require.NoError(t, err)
	third, err := Dump(ctx, db)
	require.NoError(t, err)
	require.Contains(t, strings.Join(third.Violations, "\n"), "view|keep_me_view")
	require.Contains(t, strings.Join(third.Violations, "\n"), "function|keep_me_fn")
}

// TestExpandOnlyIsNotVacuous 是 I3 判定本身的自检（不依赖 runner）：删列/改类型
// 必须红，加表加可空列必须绿。
func TestExpandOnlyIsNotVacuous(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	_, err := db.ExecContext(ctx, `CREATE TABLE public.keep_me (id bigint NOT NULL, note varchar NOT NULL DEFAULT '', PRIMARY KEY (id))`)
	require.NoError(t, err)
	before, err := Dump(ctx, db)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `ALTER TABLE public.keep_me ADD COLUMN extra text NULL`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE public.brand_new (id bigint NOT NULL, PRIMARY KEY (id))`)
	require.NoError(t, err)
	expanded, err := Dump(ctx, db)
	require.NoError(t, err)
	require.Empty(t, ExpandOnly(before, expanded))

	_, err = db.ExecContext(ctx, `ALTER TABLE public.keep_me ALTER COLUMN note TYPE text`)
	require.NoError(t, err)
	retyped, err := Dump(ctx, db)
	require.NoError(t, err)
	require.NotEmpty(t, ExpandOnly(before, retyped), "a column type change is not expand-only")
}
