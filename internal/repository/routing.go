// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
)

var routingQualityInstanceColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_quality_instance_minute_id_seq'::regclass)`,
	`identity_version smallint NOT NULL`,
	`route_class_id bytea NOT NULL`,
	`quality_class_id bytea NOT NULL`,
	`candidate_fingerprint bytea NOT NULL`,
	`instance_src text NOT NULL`,
	`bucket_minute timestamptz NOT NULL`,
	`absolute_sequence bigint NOT NULL`,
	`attempts bigint NOT NULL DEFAULT 0`,
	`successes bigint NOT NULL DEFAULT 0`,
	`failures bigint NOT NULL DEFAULT 0`,
	`ttft_sum_ms bigint NOT NULL DEFAULT 0`,
	`ttft_count bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingQualityInstanceCreateDDL = partitionedCreateDDL("routing_quality_instance_minute", "bucket_minute", routingQualityInstanceColumnDefs)

var routingQualityInstanceIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_quality_instance_minute_uniq ON routing_quality_instance_minute (instance_src, bucket_minute, candidate_fingerprint, quality_class_id, identity_version)`,
	`CREATE INDEX routing_quality_instance_minute_bucket ON routing_quality_instance_minute (bucket_minute)`,
}

var routingFlowInstanceColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_flow_instance_minute_id_seq'::regclass)`,
	`identity_version smallint NOT NULL`,
	`route_class_id bytea NOT NULL`,
	`terminal_minute timestamptz NOT NULL`,
	`ordinal smallint NOT NULL`,
	`lane text NOT NULL`,
	`account_id bigint NOT NULL`,
	`previous_account_id bigint NULL`,
	`outcome text NOT NULL`,
	`reason text NOT NULL`,
	`is_terminal boolean NOT NULL`,
	`generation bigint NOT NULL`,
	`candidate_fingerprint bytea NOT NULL`,
	`instance_src text NOT NULL`,
	`chain_count bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingFlowInstanceCreateDDL = partitionedCreateDDL("routing_flow_instance_minute", "terminal_minute", routingFlowInstanceColumnDefs)

var routingFlowInstanceIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_flow_instance_minute_uniq ON routing_flow_instance_minute (instance_src, terminal_minute, ordinal, lane, account_id, candidate_fingerprint, identity_version)`,
	`CREATE INDEX routing_flow_instance_minute_terminal ON routing_flow_instance_minute (terminal_minute)`,
}

var routingQualityRollupColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_quality_rollup_id_seq'::regclass)`,
	`identity_version smallint NOT NULL`,
	`route_class_id bytea NOT NULL`,
	`quality_class_id bytea NOT NULL`,
	`candidate_fingerprint bytea NOT NULL`,
	`bucket_minute timestamptz NOT NULL`,
	`attempts bigint NOT NULL DEFAULT 0`,
	`successes bigint NOT NULL DEFAULT 0`,
	`failures bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingQualityRollupCreateDDL = partitionedCreateDDL("routing_quality_rollup", "bucket_minute", routingQualityRollupColumnDefs)

var routingQualityRollupIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_quality_rollup_uniq ON routing_quality_rollup (bucket_minute, candidate_fingerprint, quality_class_id, identity_version)`,
}

var routingFlowRollupColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_flow_rollup_id_seq'::regclass)`,
	`identity_version smallint NOT NULL`,
	`route_class_id bytea NOT NULL`,
	`terminal_minute timestamptz NOT NULL`,
	`ordinal smallint NOT NULL`,
	`lane text NOT NULL`,
	`account_id bigint NOT NULL`,
	`previous_account_id bigint NULL`,
	`outcome text NOT NULL`,
	`reason text NOT NULL`,
	`is_terminal boolean NOT NULL`,
	`generation bigint NOT NULL`,
	`candidate_fingerprint bytea NOT NULL`,
	`chain_count bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingFlowRollupCreateDDL = partitionedCreateDDL("routing_flow_rollup", "terminal_minute", routingFlowRollupColumnDefs)

var routingFlowRollupIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_flow_rollup_uniq ON routing_flow_rollup (terminal_minute, ordinal, lane, account_id, candidate_fingerprint, identity_version)`,
}

var routingDirtyDDL = `CREATE TABLE IF NOT EXISTS routing_dirty_minute (
	bucket_minute timestamptz NOT NULL PRIMARY KEY,
	dirty boolean NOT NULL DEFAULT true,
	identity_version smallint NOT NULL DEFAULT 1,
	updated_at timestamptz NOT NULL
)`

var routingCompilerDDL = `CREATE TABLE IF NOT EXISTS routing_compiler_state (
	id bigint NOT NULL PRIMARY KEY,
	desired_generation bigint NOT NULL DEFAULT 0,
	published_generation bigint NOT NULL DEFAULT 0,
	last_error text NULL,
	updated_at timestamptz NOT NULL,
	CONSTRAINT routing_compiler_single CHECK (id = 1)
)`

func (r *PartitionRepo) EnsureRoutingQualityInstancePartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "routing_quality_instance_minute", "bucket_minute", routingQualityInstanceColumnDefs, routingQualityInstanceIndexDDLs, now)
}
func (r *PartitionRepo) EnsureRoutingFlowInstancePartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "routing_flow_instance_minute", "terminal_minute", routingFlowInstanceColumnDefs, routingFlowInstanceIndexDDLs, now)
}
func (r *PartitionRepo) EnsureRoutingQualityRollupPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "routing_quality_rollup", "bucket_minute", routingQualityRollupColumnDefs, routingQualityRollupIndexDDLs, now)
}
func (r *PartitionRepo) EnsureRoutingFlowRollupPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "routing_flow_rollup", "terminal_minute", routingFlowRollupColumnDefs, routingFlowRollupIndexDDLs, now)
}
func (r *PartitionRepo) EnsureRoutingDirty(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, routingDirtyDDL)
}
func (r *PartitionRepo) EnsureRoutingCompiler(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, routingCompilerDDL)
}

func (r *PartitionRepo) EnsureRoutingPartitions(ctx context.Context, now time.Time) error {
	if err := r.EnsureRoutingQualityInstancePartitioned(ctx, now); err != nil {
		return fmt.Errorf("routing quality instance: %w", err)
	}
	if err := r.EnsureRoutingFlowInstancePartitioned(ctx, now); err != nil {
		return fmt.Errorf("routing flow instance: %w", err)
	}
	if err := r.EnsureRoutingQualityRollupPartitioned(ctx, now); err != nil {
		return fmt.Errorf("routing quality rollup: %w", err)
	}
	if err := r.EnsureRoutingFlowRollupPartitioned(ctx, now); err != nil {
		return fmt.Errorf("routing flow rollup: %w", err)
	}
	if err := r.EnsureRoutingDirty(ctx); err != nil {
		return fmt.Errorf("routing dirty: %w", err)
	}
	if err := r.EnsureRoutingCompiler(ctx); err != nil {
		return fmt.Errorf("routing compiler: %w", err)
	}
	return nil
}

func (r *PartitionRepo) EnsureRoutingInstancePartitions(ctx context.Context, now, until time.Time) error {
	if err := r.EnsureTablePartitions(ctx, "routing_quality_instance_minute", now, until); err != nil {
		return err
	}
	if err := r.EnsureTablePartitions(ctx, "routing_flow_instance_minute", now, until); err != nil {
		return err
	}
	return nil
}
func (r *PartitionRepo) EnsureRoutingRollupPartitions(ctx context.Context, now, until time.Time) error {
	if err := r.EnsureTablePartitions(ctx, "routing_quality_rollup", now, until); err != nil {
		return err
	}
	if err := r.EnsureTablePartitions(ctx, "routing_flow_rollup", now, until); err != nil {
		return err
	}
	return nil
}

func (r *PartitionRepo) DropRoutingQualityInstanceBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "routing_quality_instance_minute", cutoff)
}
func (r *PartitionRepo) DropRoutingFlowInstanceBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "routing_flow_instance_minute", cutoff)
}
func (r *PartitionRepo) DropRoutingQualityRollupBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "routing_quality_rollup", cutoff)
}
func (r *PartitionRepo) DropRoutingFlowRollupBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "routing_flow_rollup", cutoff)
}

type RoutingQualityRow struct {
	IdentityVersion      int16
	RouteClassID         []byte
	QualityClassID       []byte
	CandidateFingerprint []byte
	InstanceSrc          string
	BucketMinute         time.Time
	AbsoluteSequence     int64
	Attempts             int64
	Successes            int64
	Failures             int64
	TTFTSumMS            int64
	TTFTCount            int64
}

type RoutingFlowRow struct {
	IdentityVersion      int16
	RouteClassID         []byte
	TerminalMinute       time.Time
	Ordinal              int16
	Lane                 string
	AccountID            int64
	PreviousAccountID    *int64
	Outcome              string
	Reason               string
	IsTerminal           bool
	Generation           int64
	CandidateFingerprint []byte
	InstanceSrc          string
	ChainCount           int64
}

func (r *PartitionRepo) UpsertQualityAndMarkDirty(ctx context.Context, row RoutingQualityRow) error {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	bucket := row.BucketMinute.UTC().Truncate(time.Minute)
	q := `INSERT INTO routing_quality_instance_minute (identity_version, route_class_id, quality_class_id, candidate_fingerprint, instance_src, bucket_minute, absolute_sequence, attempts, successes, failures, ttft_sum_ms, ttft_count, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
	ON CONFLICT (instance_src, bucket_minute, candidate_fingerprint, quality_class_id, identity_version) DO UPDATE SET
		absolute_sequence = EXCLUDED.absolute_sequence,
		attempts = EXCLUDED.attempts,
		successes = EXCLUDED.successes,
		failures = EXCLUDED.failures,
		ttft_sum_ms = EXCLUDED.ttft_sum_ms,
		ttft_count = EXCLUDED.ttft_count,
		updated_at = now()
	WHERE EXCLUDED.absolute_sequence >= routing_quality_instance_minute.absolute_sequence`
	var res sql.Result
	if err := drv.Exec(ctx, q, []any{row.IdentityVersion, row.RouteClassID, row.QualityClassID, row.CandidateFingerprint, row.InstanceSrc, bucket, row.AbsoluteSequence, row.Attempts, row.Successes, row.Failures, row.TTFTSumMS, row.TTFTCount}, &res); err != nil {
		return err
	}
	q2 := `INSERT INTO routing_dirty_minute (bucket_minute, dirty, identity_version, updated_at) VALUES ($1, true, $2, now()) ON CONFLICT (bucket_minute) DO UPDATE SET dirty = true, updated_at = now()`
	if err := drv.Exec(ctx, q2, []any{bucket, row.IdentityVersion}, &res); err != nil {
		return err
	}
	bucketLock := bucket.Unix() / 60
	qlock := `SELECT pg_advisory_xact_lock($1)`
	if err := drv.Exec(ctx, qlock, []any{bucketLock}, &res); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *PartitionRepo) UpsertFlowAndMarkDirty(ctx context.Context, row RoutingFlowRow) error {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	term := row.TerminalMinute.UTC().Truncate(time.Minute)
	q := `INSERT INTO routing_flow_instance_minute (identity_version, route_class_id, terminal_minute, ordinal, lane, account_id, previous_account_id, outcome, reason, is_terminal, generation, candidate_fingerprint, instance_src, chain_count, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, now())
	ON CONFLICT (instance_src, terminal_minute, ordinal, lane, account_id, candidate_fingerprint, identity_version) DO UPDATE SET
		chain_count = EXCLUDED.chain_count,
		updated_at = now()
	WHERE EXCLUDED.chain_count >= routing_flow_instance_minute.chain_count`
	var res sql.Result
	if err := drv.Exec(ctx, q, []any{row.IdentityVersion, row.RouteClassID, term, row.Ordinal, row.Lane, row.AccountID, row.PreviousAccountID, row.Outcome, row.Reason, row.IsTerminal, row.Generation, row.CandidateFingerprint, row.InstanceSrc, row.ChainCount}, &res); err != nil {
		return err
	}
	q2 := `INSERT INTO routing_dirty_minute (bucket_minute, dirty, identity_version, updated_at) VALUES ($1, true, $2, now()) ON CONFLICT (bucket_minute) DO UPDATE SET dirty = true, updated_at = now()`
	if err := drv.Exec(ctx, q2, []any{term, row.IdentityVersion}, &res); err != nil {
		return err
	}
	bucketLock := term.Unix() / 60
	qlock := `SELECT pg_advisory_xact_lock($1)`
	if err := drv.Exec(ctx, qlock, []any{bucketLock}, &res); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *PartitionRepo) QueryQualityRow(ctx context.Context, instanceSrc string, bucket time.Time, fingerprint []byte, qualityClass []byte, version int16) (*RoutingQualityRow, error) {
	bucket = bucket.UTC().Truncate(time.Minute)
	rows := &entsql.Rows{}
	q := `SELECT identity_version, route_class_id, quality_class_id, candidate_fingerprint, instance_src, bucket_minute, absolute_sequence, attempts, successes, failures, ttft_sum_ms, ttft_count FROM routing_quality_instance_minute WHERE instance_src=$1 AND bucket_minute=$2 AND candidate_fingerprint=$3 AND quality_class_id=$4 AND identity_version=$5`
	if err := r.driver.Query(ctx, q, []any{instanceSrc, bucket, fingerprint, qualityClass, version}, rows); err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	var out RoutingQualityRow
	var rt, qc []byte
	var fp []byte
	var bucketOut time.Time
	if err := rows.Scan(&out.IdentityVersion, &rt, &qc, &fp, &out.InstanceSrc, &bucketOut, &out.AbsoluteSequence, &out.Attempts, &out.Successes, &out.Failures, &out.TTFTSumMS, &out.TTFTCount); err != nil {
		return nil, err
	}
	out.RouteClassID = rt
	out.QualityClassID = qc
	out.CandidateFingerprint = fp
	out.BucketMinute = bucketOut
	return &out, nil
}

func (r *PartitionRepo) IsDirty(ctx context.Context, bucket time.Time) (bool, error) {
	bucket = bucket.UTC().Truncate(time.Minute)
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT dirty FROM routing_dirty_minute WHERE bucket_minute=$1`, []any{bucket}, rows); err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, nil
	}
	var d bool
	if err := rows.Scan(&d); err != nil {
		return false, err
	}
	return d, nil
}
