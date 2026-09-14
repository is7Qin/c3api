// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

var routingQualityInstanceColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_quality_instance_minute_id_seq'::regclass)`,
	`identity_version smallint NOT NULL CHECK (identity_version = 1)`,
	`route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32)`,
	`quality_class_id bytea NOT NULL CHECK (octet_length(quality_class_id) = 32)`,
	`candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32)`,
	`instance_src text NOT NULL`,
	`bucket_minute timestamptz NOT NULL`,
	`absolute_sequence bigint NOT NULL`,
	`attempts bigint NOT NULL DEFAULT 0`,
	`successes bigint NOT NULL DEFAULT 0`,
	`count_429 bigint NOT NULL DEFAULT 0`,
	`count_ordinary_4xx bigint NOT NULL DEFAULT 0`,
	`count_5xx bigint NOT NULL DEFAULT 0`,
	`count_network bigint NOT NULL DEFAULT 0`,
	`ttft_n bigint NOT NULL DEFAULT 0`,
	`ttft_sum_log_q32 bigint NOT NULL DEFAULT 0`,
	`ttft_sumsq_log_q32 bigint NOT NULL DEFAULT 0`,
	`ttft_hist bigint[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0,0,0}'`,
	`input_tokens bigint NOT NULL DEFAULT 0`,
	`output_tokens bigint NOT NULL DEFAULT 0`,
	`cache_read_tokens bigint NOT NULL DEFAULT 0`,
	`cache_create_tokens bigint NOT NULL DEFAULT 0`,
	`calls bigint NOT NULL DEFAULT 0`,
	`images bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingQualityInstanceCreateDDL = partitionedCreateDDL("routing_quality_instance_minute", "bucket_minute", routingQualityInstanceColumnDefs)

var routingQualityInstanceIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_quality_instance_minute_uniq ON routing_quality_instance_minute (instance_src, bucket_minute, candidate_fingerprint, quality_class_id, route_class_id, identity_version)`,
	`CREATE INDEX routing_quality_instance_minute_bucket ON routing_quality_instance_minute (bucket_minute)`,
}

var routingFlowInstanceColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_flow_instance_minute_id_seq'::regclass)`,
	`identity_version smallint NOT NULL CHECK (identity_version = 1)`,
	`route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32)`,
	`terminal_minute timestamptz NOT NULL`,
	`ordinal smallint NOT NULL`,
	`lane text NOT NULL`,
	`account_id bigint NOT NULL`,
	`previous_account_id bigint NULL`,
	`previous_outcome text NOT NULL DEFAULT ''`,
	`transition_reason text NOT NULL`,
	`outcome text NOT NULL`,
	`is_terminal boolean NOT NULL`,
	`generation bigint NOT NULL`,
	`candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32)`,
	`instance_src text NOT NULL`,
	`absolute_sequence bigint NOT NULL`,
	`chain_count bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingFlowInstanceCreateDDL = partitionedCreateDDL("routing_flow_instance_minute", "terminal_minute", routingFlowInstanceColumnDefs)

var routingFlowInstanceIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_flow_instance_minute_uniq ON routing_flow_instance_minute (instance_src, terminal_minute, identity_version, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, route_class_id) NULLS NOT DISTINCT`,
	`CREATE INDEX routing_flow_instance_minute_terminal ON routing_flow_instance_minute (terminal_minute)`,
}

var routingQualityRollupColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_quality_rollup_id_seq'::regclass)`,
	`identity_version smallint NOT NULL CHECK (identity_version = 1)`,
	`route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32)`,
	`quality_class_id bytea NOT NULL CHECK (octet_length(quality_class_id) = 32)`,
	`candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32)`,
	`bucket_minute timestamptz NOT NULL`,
	`attempts bigint NOT NULL DEFAULT 0`,
	`successes bigint NOT NULL DEFAULT 0`,
	`count_429 bigint NOT NULL DEFAULT 0`,
	`count_ordinary_4xx bigint NOT NULL DEFAULT 0`,
	`count_5xx bigint NOT NULL DEFAULT 0`,
	`count_network bigint NOT NULL DEFAULT 0`,
	`ttft_n bigint NOT NULL DEFAULT 0`,
	`ttft_sum_log_q32 bigint NOT NULL DEFAULT 0`,
	`ttft_sumsq_log_q32 bigint NOT NULL DEFAULT 0`,
	`ttft_hist bigint[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0,0,0}'`,
	`input_tokens bigint NOT NULL DEFAULT 0`,
	`output_tokens bigint NOT NULL DEFAULT 0`,
	`cache_read_tokens bigint NOT NULL DEFAULT 0`,
	`cache_create_tokens bigint NOT NULL DEFAULT 0`,
	`calls bigint NOT NULL DEFAULT 0`,
	`images bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingQualityRollupCreateDDL = partitionedCreateDDL("routing_quality_rollup", "bucket_minute", routingQualityRollupColumnDefs)

var routingQualityRollupIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_quality_rollup_uniq ON routing_quality_rollup (bucket_minute, candidate_fingerprint, quality_class_id, route_class_id, identity_version)`,
	`CREATE INDEX routing_quality_rollup_candidate ON routing_quality_rollup (route_class_id, candidate_fingerprint, bucket_minute DESC)`,
}

var routingFlowRollupColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_flow_rollup_id_seq'::regclass)`,
	`identity_version smallint NOT NULL CHECK (identity_version = 1)`,
	`route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32)`,
	`terminal_minute timestamptz NOT NULL`,
	`ordinal smallint NOT NULL`,
	`lane text NOT NULL`,
	`account_id bigint NOT NULL`,
	`previous_account_id bigint NULL`,
	`previous_outcome text NOT NULL DEFAULT ''`,
	`transition_reason text NOT NULL`,
	`outcome text NOT NULL`,
	`is_terminal boolean NOT NULL`,
	`generation bigint NOT NULL`,
	`candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32)`,
	`absolute_sequence bigint NOT NULL`,
	`chain_count bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingFlowRollupCreateDDL = partitionedCreateDDL("routing_flow_rollup", "terminal_minute", routingFlowRollupColumnDefs)

var routingFlowRollupIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_flow_rollup_uniq ON routing_flow_rollup (terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, route_class_id, identity_version) NULLS NOT DISTINCT`,
}

var routingDirtyDDL = `CREATE TABLE IF NOT EXISTS routing_dirty_minute (
	kind text NOT NULL,
	identity_version smallint NOT NULL CHECK (identity_version = 1),
	bucket_minute timestamptz NOT NULL,
	dirty boolean NOT NULL DEFAULT true,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (kind, identity_version, bucket_minute)
)`

var routingWatermarkDDL = `CREATE TABLE IF NOT EXISTS routing_rollup_watermark (
	kind text NOT NULL,
	identity_version smallint NOT NULL CHECK (identity_version = 1),
	watermark timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (kind, identity_version)
)`

var routingFlowSnapshotStateDDL = `CREATE TABLE IF NOT EXISTS routing_flow_snapshot_state (
	terminal_minute timestamptz NOT NULL,
	instance_src text NOT NULL,
	identity_version smallint NOT NULL CHECK (identity_version = 1),
	highest_sequence bigint NOT NULL,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (terminal_minute, instance_src, identity_version)
)`

var routingCompilerDDL = `CREATE TABLE IF NOT EXISTS routing_compiler_state (
	id bigint NOT NULL,
	identity_version smallint NOT NULL CHECK (identity_version = 1),
	desired_generation bigint NOT NULL DEFAULT 0,
	published_generation bigint NOT NULL DEFAULT 0,
	last_error text NULL,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (id, identity_version),
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
func (r *PartitionRepo) EnsureRoutingWatermark(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, routingWatermarkDDL)
}
func (r *PartitionRepo) EnsureRoutingSnapshotState(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, routingFlowSnapshotStateDDL)
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
	if err := r.EnsureRoutingWatermark(ctx); err != nil {
		return fmt.Errorf("routing watermark: %w", err)
	}
	if err := r.EnsureRoutingSnapshotState(ctx); err != nil {
		return fmt.Errorf("routing snapshot state: %w", err)
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
	RouteClassID         domain.RouteClassIDVal
	QualityClassID       domain.QualityClassIDVal
	CandidateFingerprint domain.CandidateFingerprintVal
	InstanceSrc          string
	BucketMinute         time.Time
	AbsoluteSequence     int64
	Attempts             int64
	Successes            int64
	Count429             int64
	CountOrdinary4xx     int64
	Count5xx             int64
	CountNetwork         int64
	TTFTN                int64
	TTFTSumLogQ32        int64
	TTFTSumSqLogQ32      int64
	TTFTHist             []int64
	InputTokens          int64
	OutputTokens         int64
	CacheReadTokens      int64
	CacheCreateTokens    int64
	Calls                int64
	Images               int64
}

type RoutingFlowRow struct {
	IdentityVersion      int16
	RouteClassID         domain.RouteClassIDVal
	TerminalMinute       time.Time
	Ordinal              int16
	Lane                 string
	AccountID            int64
	PreviousAccountID    *int64
	PreviousOutcome      string
	TransitionReason     string
	Outcome              string
	IsTerminal           bool
	Generation           int64
	CandidateFingerprint domain.CandidateFingerprintVal
	InstanceSrc          string
	AbsoluteSequence     int64
	ChainCount           int64
}

func advisoryLockKey(parts ...string) int64 {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (r *PartitionRepo) UpsertQualityAndMarkDirty(ctx context.Context, row RoutingQualityRow) error {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	bucket := row.BucketMinute.UTC().Truncate(time.Minute)
	lockKey := advisoryLockKey("quality", fmt.Sprintf("%d", row.IdentityVersion), bucket.Format(time.RFC3339), row.InstanceSrc)
	var res sql.Result
	if err := drv.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{lockKey}, &res); err != nil {
		return err
	}
	q := `INSERT INTO routing_quality_instance_minute (identity_version, route_class_id, quality_class_id, candidate_fingerprint, instance_src, bucket_minute, absolute_sequence, attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network, ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23, now())
	ON CONFLICT (instance_src, bucket_minute, candidate_fingerprint, quality_class_id, route_class_id, identity_version) DO UPDATE SET
		absolute_sequence = EXCLUDED.absolute_sequence,
		attempts = EXCLUDED.attempts,
		successes = EXCLUDED.successes,
		count_429 = EXCLUDED.count_429,
		count_ordinary_4xx = EXCLUDED.count_ordinary_4xx,
		count_5xx = EXCLUDED.count_5xx,
		count_network = EXCLUDED.count_network,
		ttft_n = EXCLUDED.ttft_n,
		ttft_sum_log_q32 = EXCLUDED.ttft_sum_log_q32,
		ttft_sumsq_log_q32 = EXCLUDED.ttft_sumsq_log_q32,
		ttft_hist = EXCLUDED.ttft_hist,
		input_tokens = EXCLUDED.input_tokens,
		output_tokens = EXCLUDED.output_tokens,
		cache_read_tokens = EXCLUDED.cache_read_tokens,
		cache_create_tokens = EXCLUDED.cache_create_tokens,
		calls = EXCLUDED.calls,
		images = EXCLUDED.images,
		updated_at = now()
	WHERE EXCLUDED.absolute_sequence > routing_quality_instance_minute.absolute_sequence`
	if row.TTFTHist == nil {
		row.TTFTHist = []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	if err := drv.Exec(ctx, q, []any{row.IdentityVersion, row.RouteClassID[:], row.QualityClassID[:], row.CandidateFingerprint[:], row.InstanceSrc, bucket, row.AbsoluteSequence, row.Attempts, row.Successes, row.Count429, row.CountOrdinary4xx, row.Count5xx, row.CountNetwork, row.TTFTN, row.TTFTSumLogQ32, row.TTFTSumSqLogQ32, row.TTFTHist, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreateTokens, row.Calls, row.Images}, &res); err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	q2 := `INSERT INTO routing_dirty_minute (kind, identity_version, bucket_minute, dirty, updated_at) VALUES ('quality', $1, $2, true, now()) ON CONFLICT (kind, identity_version, bucket_minute) DO UPDATE SET dirty = true, updated_at = now()`
	if err := drv.Exec(ctx, q2, []any{row.IdentityVersion, bucket}, &res); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertFlowSnapshot replaces the complete edge set for (terminal_minute, instance_src, identity_version) atomically.
// Only greater absolute_sequence replaces; equal or lower does not mutate. Uses durable authority table routing_flow_snapshot_state
// so even empty snapshots advance sequence and remain authoritative independent of edge rows.
func (r *PartitionRepo) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, identityVersion int16, absoluteSequence int64, rows []RoutingFlowRow) error {
	terminalMinute = terminalMinute.UTC().Truncate(time.Minute)
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	lockKey := advisoryLockKey("flow", fmt.Sprintf("%d", identityVersion), terminalMinute.Format(time.RFC3339), instanceSrc)
	var res sql.Result
	if err := drv.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{lockKey}, &res); err != nil {
		return err
	}
	var curSeq sql.NullInt64
	rs := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT highest_sequence FROM routing_flow_snapshot_state WHERE terminal_minute=$1 AND instance_src=$2 AND identity_version=$3 FOR UPDATE`, []any{terminalMinute, instanceSrc, identityVersion}, rs); err != nil {
		return err
	}
	hasState := rs.Next()
	if hasState {
		_ = rs.Scan(&curSeq)
	}
	rs.Close()
	if hasState && curSeq.Valid && absoluteSequence <= curSeq.Int64 {
		return tx.Commit()
	}
	if err := drv.Exec(ctx, `DELETE FROM routing_flow_instance_minute WHERE terminal_minute=$1 AND instance_src=$2 AND identity_version=$3`, []any{terminalMinute, instanceSrc, identityVersion}, &res); err != nil {
		return err
	}
	for _, row := range rows {
		q := `INSERT INTO routing_flow_instance_minute (identity_version, route_class_id, terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, instance_src, absolute_sequence, chain_count, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, now())`
		if err := drv.Exec(ctx, q, []any{identityVersion, row.RouteClassID[:], terminalMinute, row.Ordinal, row.Lane, row.AccountID, row.PreviousAccountID, row.PreviousOutcome, row.TransitionReason, row.Outcome, row.IsTerminal, row.Generation, row.CandidateFingerprint[:], instanceSrc, absoluteSequence, row.ChainCount}, &res); err != nil {
			return err
		}
	}
	if hasState {
		if err := drv.Exec(ctx, `UPDATE routing_flow_snapshot_state SET highest_sequence=$1, updated_at=now() WHERE terminal_minute=$2 AND instance_src=$3 AND identity_version=$4`, []any{absoluteSequence, terminalMinute, instanceSrc, identityVersion}, &res); err != nil {
			return err
		}
	} else {
		if err := drv.Exec(ctx, `INSERT INTO routing_flow_snapshot_state (terminal_minute, instance_src, identity_version, highest_sequence, updated_at) VALUES ($1,$2,$3,$4, now())`, []any{terminalMinute, instanceSrc, identityVersion, absoluteSequence}, &res); err != nil {
			return err
		}
	}
	q2 := `INSERT INTO routing_dirty_minute (kind, identity_version, bucket_minute, dirty, updated_at) VALUES ('flow', $1, $2, true, now()) ON CONFLICT (kind, identity_version, bucket_minute) DO UPDATE SET dirty = true, updated_at = now()`
	if err := drv.Exec(ctx, q2, []any{identityVersion, terminalMinute}, &res); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *PartitionRepo) QueryQualityRow(ctx context.Context, instanceSrc string, bucket time.Time, fingerprint domain.CandidateFingerprintVal, qualityClass domain.QualityClassIDVal, routeClass domain.RouteClassIDVal, version int16) (*RoutingQualityRow, error) {
	bucket = bucket.UTC().Truncate(time.Minute)
	rows := &entsql.Rows{}
	q := `SELECT identity_version, route_class_id, quality_class_id, candidate_fingerprint, instance_src, bucket_minute, absolute_sequence, attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network, ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist::text, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images FROM routing_quality_instance_minute WHERE instance_src=$1 AND bucket_minute=$2 AND candidate_fingerprint=$3 AND quality_class_id=$4 AND route_class_id=$5 AND identity_version=$6`
	if err := r.driver.Query(ctx, q, []any{instanceSrc, bucket, fingerprint[:], qualityClass[:], routeClass[:], version}, rows); err != nil {
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
	var histText string
	if err := rows.Scan(&out.IdentityVersion, &rt, &qc, &fp, &out.InstanceSrc, &bucketOut, &out.AbsoluteSequence, &out.Attempts, &out.Successes, &out.Count429, &out.CountOrdinary4xx, &out.Count5xx, &out.CountNetwork, &out.TTFTN, &out.TTFTSumLogQ32, &out.TTFTSumSqLogQ32, &histText, &out.InputTokens, &out.OutputTokens, &out.CacheReadTokens, &out.CacheCreateTokens, &out.Calls, &out.Images); err != nil {
		return nil, err
	}
	copy(out.RouteClassID[:], rt)
	copy(out.QualityClassID[:], qc)
	copy(out.CandidateFingerprint[:], fp)
	out.BucketMinute = bucketOut
	out.TTFTHist = parseRoutingHist(histText)
	return &out, nil
}

func (r *PartitionRepo) IsDirty(ctx context.Context, kind string, version int16, bucket time.Time) (bool, error) {
	bucket = bucket.UTC().Truncate(time.Minute)
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT dirty FROM routing_dirty_minute WHERE kind=$1 AND identity_version=$2 AND bucket_minute=$3`, []any{kind, version, bucket}, rows); err != nil {
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

// ListDirtyMinutes 是 rollup worker 的最小选择缝：返回 kind/version 下
// dirty=true 的最老分钟（升序、至多 limit 个）。from 仍作为调用方的进度
// 观测参数保留；迟到分钟也必须先被消费，不能被更新的 watermark 跳过。
func (r *PartitionRepo) ListDirtyMinutes(ctx context.Context, kind string, version int16, from time.Time, limit int) ([]time.Time, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT bucket_minute FROM routing_dirty_minute WHERE kind=$1 AND identity_version=$2 AND dirty=true ORDER BY bucket_minute ASC LIMIT $3`, []any{kind, version, limit}, rows); err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []time.Time{}
	for rows.Next() {
		var b time.Time
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *PartitionRepo) GetWatermark(ctx context.Context, kind string, version int16) (time.Time, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT watermark FROM routing_rollup_watermark WHERE kind=$1 AND identity_version=$2`, []any{kind, version}, rows); err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return time.Time{}, sql.ErrNoRows
	}
	var w time.Time
	if err := rows.Scan(&w); err != nil {
		return time.Time{}, err
	}
	return w, nil
}

func (r *PartitionRepo) advanceWatermarkTx(ctx context.Context, drv *txDriver, kind string, version int16, newWatermark time.Time) error {
	newWatermark = newWatermark.UTC().Truncate(time.Minute)
	var res sql.Result
	var cur sql.NullTime
	rs := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT watermark FROM routing_rollup_watermark WHERE kind=$1 AND identity_version=$2 FOR UPDATE`, []any{kind, version}, rs); err != nil {
		return err
	}
	has := rs.Next()
	if has {
		var w time.Time
		_ = rs.Scan(&w)
		cur = sql.NullTime{Time: w, Valid: true}
	}
	rs.Close()
	if cur.Valid && newWatermark.Before(cur.Time) {
		if err := drv.Exec(ctx, `UPDATE routing_dirty_minute SET dirty=false, updated_at=now() WHERE kind=$1 AND identity_version=$2 AND bucket_minute=$3 AND dirty=true`, []any{kind, version, newWatermark}, &res); err != nil {
			return err
		}
		return nil
	}
	if has {
		if err := drv.Exec(ctx, `UPDATE routing_rollup_watermark SET watermark=$1, updated_at=now() WHERE kind=$2 AND identity_version=$3`, []any{newWatermark, kind, version}, &res); err != nil {
			return err
		}
	} else {
		if err := drv.Exec(ctx, `INSERT INTO routing_rollup_watermark (kind, identity_version, watermark, updated_at) VALUES ($1,$2,$3, now())`, []any{kind, version, newWatermark}, &res); err != nil {
			return err
		}
	}
	if err := drv.Exec(ctx, `UPDATE routing_dirty_minute SET dirty=false, updated_at=now() WHERE kind=$1 AND identity_version=$2 AND bucket_minute=$3 AND dirty=true`, []any{kind, version, newWatermark}, &res); err != nil {
		return err
	}
	return nil
}

func (r *PartitionRepo) RollupQuality(ctx context.Context, bucket time.Time, version int16) error {
	bucket = bucket.UTC().Truncate(time.Minute)
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	lockKey := advisoryLockKey("rollup-quality", fmt.Sprintf("%d", version), bucket.Format(time.RFC3339))
	var res sql.Result
	if err := drv.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{lockKey}, &res); err != nil {
		return err
	}
	// lock dirty row FOR UPDATE before reading facts (no-lost-dirty)
	dirtyRows := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT dirty FROM routing_dirty_minute WHERE kind='quality' AND identity_version=$1 AND bucket_minute=$2 FOR UPDATE`, []any{version, bucket}, dirtyRows); err != nil {
		return err
	}
	hasDirty := dirtyRows.Next()
	var isDirty bool
	if hasDirty {
		_ = dirtyRows.Scan(&isDirty)
	}
	dirtyRows.Close()
	if !hasDirty || !isDirty {
		return fmt.Errorf("rollup requires dirty minute %v", bucket)
	}
	// read facts existence (must have at least one row)
	factRows := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT 1 FROM routing_quality_instance_minute WHERE bucket_minute=$1 AND identity_version=$2 LIMIT 1`, []any{bucket, version}, factRows); err != nil {
		return err
	}
	hasFact := factRows.Next()
	factRows.Close()
	if !hasFact {
		return fmt.Errorf("no facts for rollup")
	}
	if err := drv.Exec(ctx, `INSERT INTO routing_quality_rollup (identity_version, route_class_id, quality_class_id, candidate_fingerprint, bucket_minute, attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network, ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images, updated_at)
	WITH grouped AS (
		SELECT identity_version, route_class_id, quality_class_id, candidate_fingerprint, bucket_minute,
			MAX(absolute_sequence) AS absolute_sequence,
			SUM(attempts) AS attempts, SUM(successes) AS successes,
			SUM(count_429) AS count_429, SUM(count_ordinary_4xx) AS count_ordinary_4xx,
			SUM(count_5xx) AS count_5xx, SUM(count_network) AS count_network,
			SUM(ttft_n) AS ttft_n, SUM(ttft_sum_log_q32) AS ttft_sum_log_q32,
			SUM(ttft_sumsq_log_q32) AS ttft_sumsq_log_q32,
			array_agg(ttft_hist) AS hist_rows,
			SUM(input_tokens) AS input_tokens, SUM(output_tokens) AS output_tokens,
			SUM(cache_read_tokens) AS cache_read_tokens, SUM(cache_create_tokens) AS cache_create_tokens,
			SUM(calls) AS calls, SUM(images) AS images
		FROM routing_quality_instance_minute
		WHERE bucket_minute=$1 AND identity_version=$2
		GROUP BY identity_version, route_class_id, quality_class_id, candidate_fingerprint, bucket_minute
	)
	SELECT identity_version, route_class_id, quality_class_id, candidate_fingerprint, bucket_minute,
		attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network,
		ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32,
		ARRAY(SELECT COALESCE(SUM(v), 0) FROM unnest(hist_rows) WITH ORDINALITY AS bins(v, ord)
			GROUP BY ((ord - 1) % 10) ORDER BY ((ord - 1) % 10)),
		input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images, now()
	FROM grouped
	ON CONFLICT (bucket_minute, candidate_fingerprint, quality_class_id, route_class_id, identity_version) DO UPDATE SET attempts=EXCLUDED.attempts, successes=EXCLUDED.successes, count_429=EXCLUDED.count_429, count_ordinary_4xx=EXCLUDED.count_ordinary_4xx, count_5xx=EXCLUDED.count_5xx, count_network=EXCLUDED.count_network, ttft_n=EXCLUDED.ttft_n, ttft_sum_log_q32=EXCLUDED.ttft_sum_log_q32, ttft_sumsq_log_q32=EXCLUDED.ttft_sumsq_log_q32, ttft_hist=EXCLUDED.ttft_hist, input_tokens=EXCLUDED.input_tokens, output_tokens=EXCLUDED.output_tokens, cache_read_tokens=EXCLUDED.cache_read_tokens, cache_create_tokens=EXCLUDED.cache_create_tokens, calls=EXCLUDED.calls, images=EXCLUDED.images, updated_at=now()`, []any{bucket, version}, &res); err != nil {
		return err
	}
	if err := r.advanceWatermarkTx(ctx, drv, "quality", version, bucket); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *PartitionRepo) RollupFlow(ctx context.Context, terminalMinute time.Time, version int16) error {
	terminalMinute = terminalMinute.UTC().Truncate(time.Minute)
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	lockKey := advisoryLockKey("rollup-flow", fmt.Sprintf("%d", version), terminalMinute.Format(time.RFC3339))
	var res sql.Result
	if err := drv.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{lockKey}, &res); err != nil {
		return err
	}
	dirtyRows := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT dirty FROM routing_dirty_minute WHERE kind='flow' AND identity_version=$1 AND bucket_minute=$2 FOR UPDATE`, []any{version, terminalMinute}, dirtyRows); err != nil {
		return err
	}
	hasDirty := dirtyRows.Next()
	var isDirty bool
	if hasDirty {
		_ = dirtyRows.Scan(&isDirty)
	}
	dirtyRows.Close()
	if !hasDirty || !isDirty {
		return fmt.Errorf("rollup requires dirty minute %v", terminalMinute)
	}
	factRows := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT 1 FROM routing_flow_instance_minute WHERE terminal_minute=$1 AND identity_version=$2 LIMIT 1`, []any{terminalMinute, version}, factRows); err != nil {
		return err
	}
	hasFact := factRows.Next()
	factRows.Close()
	snapRows := &entsql.Rows{}
	hasSnap := false
	if !hasFact {
		if err := drv.Query(ctx, `SELECT 1 FROM routing_flow_snapshot_state WHERE terminal_minute=$1 AND identity_version=$2 LIMIT 1`, []any{terminalMinute, version}, snapRows); err != nil {
			return err
		}
		hasSnap = snapRows.Next()
		snapRows.Close()
		if !hasSnap {
			return fmt.Errorf("no facts for rollup")
		}
	}
	if hasFact {
		if err := drv.Exec(ctx, `DELETE FROM routing_flow_rollup WHERE terminal_minute=$1 AND identity_version=$2`, []any{terminalMinute, version}, &res); err != nil {
			return err
		}
		if err := drv.Exec(ctx, `INSERT INTO routing_flow_rollup (identity_version, route_class_id, terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, absolute_sequence, chain_count, updated_at)
	SELECT identity_version, route_class_id, terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, MAX(absolute_sequence), SUM(chain_count), now() FROM routing_flow_instance_minute WHERE terminal_minute=$1 AND identity_version=$2 GROUP BY identity_version, route_class_id, terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint`, []any{terminalMinute, version}, &res); err != nil {
			return err
		}
	} else if hasSnap {
		if err := drv.Exec(ctx, `DELETE FROM routing_flow_rollup WHERE terminal_minute=$1 AND identity_version=$2`, []any{terminalMinute, version}, &res); err != nil {
			return err
		}
	}
	if err := r.advanceWatermarkTx(ctx, drv, "flow", version, terminalMinute); err != nil {
		return err
	}
	return tx.Commit()
}
