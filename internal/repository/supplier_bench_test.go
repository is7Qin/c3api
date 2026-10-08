// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// supplier_bench_test.go A17 性能阈值（spec 2026-10-09 §7.1 P1–P3；P4 全链写放大
// 由编排者手动执行，不进本文件）。**条件性**：需真实 PG（TEST_DATABASE_URL），
// 未设则 Skip；CI 不可满足。
//
// 运行示例：
//
//	TEST_DATABASE_URL=postgres://... go test ./internal/repository/ \
//	  -run '^$' -bench 'BenchmarkSupplier' -benchmem -benchtime 100x -count 3
//
// 阈值判据见 §7.1：P1 追平率 ≥ 到达率；P2 INSERT/billed-mark P99 ≤ 基线×1.10；
// P3 单次装载 P99 ≤ 50ms 且 B/op ≤ 8e6（万级 accounts）。

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/is7qin/c3api/internal/repository"
)

// benchSetup 建独立 schema 的仓库 + 池（bench 专用；随 B 结束清理）。
func benchSetup(b *testing.B) (*repository.Repository, *pgxpool.Pool) {
	b.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL not set; skipping A17 supplier benchmark")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("c3api_supplier_bench_%d_%d", os.Getpid(), time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		b.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		b.Fatalf("pool: %v", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		b.Fatalf("drop schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		b.Fatalf("create schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		b.Fatalf("set search_path: %v", err)
	}
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	if err != nil {
		b.Fatalf("new repos: %v", err)
	}
	if err := repos.EnsureUsageLogPartitioned(ctx, time.Now()); err != nil {
		b.Fatalf("partition: %v", err)
	}
	b.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_ = db.Close()
		pool.Close()
	})
	return repos, pool
}

func benchExec(b *testing.B, pool *pgxpool.Pool, query string, args ...any) {
	b.Helper()
	if _, err := pool.Exec(context.Background(), query, args...); err != nil {
		b.Fatalf("exec %q: %v", query, err)
	}
}

// p99 从样本取 nearest-rank P99。
func p99(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	idx := int(float64(len(samples))*0.99) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}

// BenchmarkSupplierCreditThroughput P1：记账链追平率——批量取批 + 记账事务吞吐
// （rows/s）。backlog 排空后补齐（真实重播被守卫拒绝，故用新行补充）。
func BenchmarkSupplierCreditThroughput(b *testing.B) {
	repos, pool := benchSetup(b)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())
	const uid = int64(90001)
	benchExec(b, pool, `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, lifetime_paid, share_bp, freeze_hours, created_at, updated_at)
		VALUES ($1,0,0,0,1000,24,now(),now())`, uid)

	seq := 0
	seed := func(n int) {
		q := `INSERT INTO usage_logs (request_id, model, format, error_type, cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed) VALUES `
		args := make([]any, 0, n*5)
		now := time.Now().UTC()
		for i := 0; i < n; i++ {
			seq++
			if i > 0 {
				q += ","
			}
			base := len(args)
			q += fmt.Sprintf("($%d,'m','openai-chat','none',$%d,$%d,$%d,$%d,false,true)", base+1, base+2, base+3, base+4, base+5)
			args = append(args, fmt.Sprintf("bench-%d-%d", uid, seq), int64(1000), now.Add(-time.Second), uid, int64(100))
		}
		benchExec(b, pool, q, args...)
	}

	const batch = 1000
	seed(batch * 4)
	// applied 只累计**真正提交**的记账行数：空批轮（补种数据）不计入吞吐，
	// 否则 rows/s 会把没有 apply 的迭代也当作满批而高估（评审：空批补数据
	// 不应计入吞吐）。补种发生在计时窗内，其成本如实落在 elapsed 上。
	var applied int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := sr.FetchCreditBatch(ctx, batch)
		if err != nil {
			b.Fatalf("fetch: %v", err)
		}
		if len(rows) == 0 {
			seed(batch * 4)
			continue
		}
		freeze, err := sr.FreezeHoursByUID(ctx, []int64{uid})
		if err != nil {
			b.Fatalf("freeze hours: %v", err)
		}
		if err := sr.ApplyCreditTx(ctx, rows, freeze); err != nil {
			b.Fatalf("apply: %v", err)
		}
		applied += int64(len(rows))
	}
	b.StopTimer()
	if secs := b.Elapsed().Seconds(); secs > 0 && applied > 0 {
		b.ReportMetric(float64(applied)/secs, "rows/s")
	}
}

// BenchmarkSupplierUsageInsertAndMark P2：启用态索引维护成本——INSERT 与 billed
// 标记 UPDATE 的 P99（off = 无 supplier 归属；on = 有归属正收益，落消费索引①）。
// 以客户端计时采样（pg_stat_statements 交叉验证由编排者手动执行）。
func BenchmarkSupplierUsageInsertAndMark(b *testing.B) {
	_, pool := benchSetup(b)
	now := time.Now().UTC()
	const n = 2000

	offIns := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		benchExec(b, pool, `INSERT INTO usage_logs (request_id, model, format, error_type, cost, created_at, supplier_credited, billed)
			VALUES ($1,'m','openai-chat','none',10,$2,true,false)`, fmt.Sprintf("off-%d", i), now)
		offIns = append(offIns, time.Since(t0))
	}
	onIns := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		benchExec(b, pool, `INSERT INTO usage_logs (request_id, model, format, error_type, cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed)
			VALUES ($1,'m','openai-chat','none',1000,$2,90001,100,false,false)`, fmt.Sprintf("on-%d", i), now)
		onIns = append(onIns, time.Since(t0))
	}
	offMark := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		benchExec(b, pool, `UPDATE usage_logs SET billed = true WHERE request_id = $1 AND NOT billed`, fmt.Sprintf("off-%d", i))
		offMark = append(offMark, time.Since(t0))
	}
	onMark := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		benchExec(b, pool, `UPDATE usage_logs SET billed = true WHERE request_id = $1 AND NOT billed`, fmt.Sprintf("on-%d", i))
		onMark = append(onMark, time.Since(t0))
	}
	b.ReportMetric(float64(p99(offIns).Microseconds()), "off_insert_p99_us")
	b.ReportMetric(float64(p99(onIns).Microseconds()), "on_insert_p99_us")
	b.ReportMetric(float64(p99(offMark).Microseconds()), "off_mark_p99_us")
	b.ReportMetric(float64(p99(onMark).Microseconds()), "on_mark_p99_us")
}

// BenchmarkSupplierSnapshotLoad P3：快照装载成本——万级 accounts 单次全量装载。
// -benchmem 记录 B/op；P99 wall-clock 由编排者采样，此处报 ns/op。
func BenchmarkSupplierSnapshotLoad(b *testing.B) {
	repos, pool := benchSetup(b)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	const accounts = 10000
	const suppliers = 100
	benchExec(b, pool, `INSERT INTO users (email, password_hash, role, status, created_at, updated_at)
		SELECT 'bench-u-'||g, 'h', 'supplier', 'active', now(), now() FROM generate_series(1,$1) g`, suppliers)
	benchExec(b, pool, `INSERT INTO templates (name, base_url, supported_formats, models, format_models, model_mapping, created_at, updated_at)
		VALUES ('bench-tpl','https://u/v1','["openai-chat"]'::jsonb,'[]'::jsonb,'{}'::jsonb,'{}'::jsonb, now(), now())`)
	benchExec(b, pool, `INSERT INTO accounts (name, template_id, upstream_key, max_concurrency, enabled, supplier_user_id, created_at, updated_at, lifecycle_revision, identity_revision)
		SELECT 'bench-a-'||g, (SELECT id FROM templates LIMIT 1), 'sk', 8, true,
		       CASE WHEN g % 2 = 0 THEN ((g % $2) + 1) ELSE NULL END, now(), now(), 1, 1
		FROM generate_series(1,$1) g`, accounts, suppliers)
	benchExec(b, pool, `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, lifetime_paid, share_bp, created_at, updated_at)
		SELECT id, 0, 0, 0, 1000, now(), now() FROM users WHERE role='supplier'`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := sr.LoadSupplierView(ctx); err != nil {
			b.Fatalf("load view: %v", err)
		}
	}
}
