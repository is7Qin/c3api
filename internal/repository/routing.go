// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

// routingQualityFactColumnDefs 是单一分片事实表 S1 的列定义事实源：身份列
// **改序**为 (route_class_id, candidate_fingerprint, bucket_minute, instance_src,
// quality_class_id)。改序不是审美：身份索引因此以
// (route_class_id, candidate_fingerprint, bucket_minute) 开头，同时服务基线窗
// LATERAL 探针（rc 等值 + fp 等值 + 分钟范围）与唯一性；把 bucket_minute 放首位
// 会迫使另建一条含两个 32 字节 bytea 的探针索引（实测 781.8 → 652.1 B/行，
// −16.6%，见 .omo/evidence/routing-footprint/README.md §4）。
//
// 旧 schema 的常量版本列已删除（§3.2 独立提交）：常量伪装成维度只膨胀每张表的
// 索引与每个读的参数；版本化由身份哈希首字节承担（domain.hashFields 写入
// RoutingIdentityVersion），DB 列并不携带额外身份信息。
var routingQualityFactColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_quality_fact_id_seq'::regclass)`,
	`route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32)`,
	`candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32)`,
	`bucket_minute timestamptz NOT NULL`,
	`instance_src text NOT NULL`,
	`quality_class_id bytea NOT NULL CHECK (octet_length(quality_class_id) = 32)`,
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

var routingQualityFactCreateDDL = partitionedCreateDDL("routing_quality_fact", "bucket_minute", routingQualityFactColumnDefs)

// routingQualityFactIndexDDLs：身份唯一索引以 (route_class_id,
// candidate_fingerprint, bucket_minute) 开头——**同时**承担唯一性约束与基线窗
// LATERAL 探针；bucket 单列索引服务 5m 当前窗（无 rc 过滤）。
var routingQualityFactIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_quality_fact_uniq ON routing_quality_fact (route_class_id, candidate_fingerprint, bucket_minute, instance_src, quality_class_id)`,
	`CREATE INDEX routing_quality_fact_bucket ON routing_quality_fact (bucket_minute)`,
}

// routing_flow_instance_minute 已随 S2′/S3 删除：实例层与 rollup 层合并为
// routing_flow_fact 单层（instance_src 为身份维度），不再有独立暂存表。

// routing_flow_fact 即合并流层 S2′：旧边身份减 generation、
// candidate_fingerprint，加 instance_src（分片身份）。chain_count 为事件累加
// 的精确和，非负不变式由写面累加语义保证，DB 不加 CHECK（Beta 无迁移路径）。
// absolute_sequence 已删除：该列只写不读，片级写守卫读的是
// routing_flow_snapshot_state.highest_sequence，列本身无任何读取方。
var routingFlowFactColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('routing_flow_fact_id_seq'::regclass)`,
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
	`instance_src text NOT NULL`,
	`min_generation bigint NOT NULL`,
	`chain_count bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var routingFlowFactCreateDDL = partitionedCreateDDL("routing_flow_fact", "terminal_minute", routingFlowFactColumnDefs)

var routingFlowFactIndexDDLs = []string{
	`CREATE UNIQUE INDEX routing_flow_fact_uniq ON routing_flow_fact (terminal_minute, instance_src, route_class_id, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal) NULLS NOT DISTINCT`,
	`CREATE INDEX routing_flow_fact_read ON routing_flow_fact (route_class_id, terminal_minute)`,
}

// ErrRoutingSnapshotBeyondRetention 快照分钟早于观测保留截止（§5.4 写面守卫）。
// 与"旧序号被拒"的幂等 no-op 语义**不同**：那条路径的 payload 已经落库过
// （tx.Commit 返回 nil 正确），而超期拒绝意味着该分钟的链已被保留策略丢弃，
// 是数据丢失。故必须显式返回本哨兵，调用方不得照抄静默形状——否则合法迟到
// 实例的链会无痕消失（判据 B5）。
var ErrRoutingSnapshotBeyondRetention = errors.New("routing flow snapshot beyond observation retention")

var routingFlowSnapshotStateDDL = `CREATE TABLE IF NOT EXISTS routing_flow_snapshot_state (
	terminal_minute timestamptz NOT NULL,
	instance_src text NOT NULL,
	highest_sequence bigint NOT NULL,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (terminal_minute, instance_src)
)`

func (r *PartitionRepo) EnsureRoutingQualityFactPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "routing_quality_fact", "bucket_minute", routingQualityFactColumnDefs, routingQualityFactIndexDDLs, now)
}
func (r *PartitionRepo) EnsureRoutingFlowFactPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "routing_flow_fact", "terminal_minute", routingFlowFactColumnDefs, routingFlowFactIndexDDLs, now)
}
func (r *PartitionRepo) EnsureRoutingSnapshotState(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, routingFlowSnapshotStateDDL)
}
func (r *PartitionRepo) EnsureRoutingPartitions(ctx context.Context, now time.Time) error {
	if err := r.EnsureRoutingQualityFactPartitioned(ctx, now); err != nil {
		return fmt.Errorf("routing quality fact: %w", err)
	}
	if err := r.EnsureRoutingFlowFactPartitioned(ctx, now); err != nil {
		return fmt.Errorf("routing flow fact: %w", err)
	}
	if err := r.EnsureRoutingSnapshotState(ctx); err != nil {
		return fmt.Errorf("routing snapshot state: %w", err)
	}
	return nil
}

// EnsureRoutingFactPartitions 预建两张事实表的未来日分区：S1
// routing_quality_fact（bucket_minute 分区）与 S2 routing_flow_fact
// （terminal_minute 分区）同属观测事实面，同一保留巡检统一调度。
func (r *PartitionRepo) EnsureRoutingFactPartitions(ctx context.Context, now, until time.Time) error {
	if err := r.EnsureTablePartitions(ctx, "routing_quality_fact", now, until); err != nil {
		return err
	}
	if err := r.EnsureTablePartitions(ctx, "routing_flow_fact", now, until); err != nil {
		return err
	}
	return nil
}

func (r *PartitionRepo) DropRoutingQualityFactBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "routing_quality_fact", cutoff)
}
func (r *PartitionRepo) DropRoutingFlowFactBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "routing_flow_fact", cutoff)
}

// RoutingFactStats 是 domain.RoutingPartitionStats 的别名（本包内的惯用名）。
// 形状定义在 domain 叶子包，而不是这里：usage 的 PartitionManager 窄接缝要跨包
// 引用它，而 usage 不得为此 import repository（依赖方向纪律见
// domain/routing_window.go 的 RoutingPartitionStats 注释）。
type RoutingFactStats = domain.RoutingPartitionStats

// CountRoutingFlowSnapshotState 返回交接状态表当前总行数（兜底观测读面）。小而
// 有界（保留天数 × 实例数，分钟级行），COUNT(*) 零成本，可随巡检每轮调用——
// 但仅限本表：事实表行数绝不得如此观测（分区 DROP 才是其有界机制，行级 COUNT
// 是全表扫描）。
func (r *PartitionRepo) CountRoutingFlowSnapshotState(ctx context.Context) (int, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT COUNT(*) FROM routing_flow_snapshot_state`, []any{}, rows); err != nil {
		return 0, err
	}
	defer rows.Close()
	n, err := entsql.ScanInt(rows)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RoutingFactPartitionStats 路由观测两张事实表的分区概况：聚合分区总数 + 最老
// 分区下界（无分区时 count=0、oldest 为零值）+ 分表计数/最老 + 快照行数 +
// 无名分区数。这是保留期**兜底**的读面——
// 事实表的有界性完全依赖保留 worker 在跑（与已下线的重算机械同一种依赖形状），
// 故「worker 停了」必须可观测，否则分区静默无界增长（spec §6/A17）。
// 只读元数据 + 一次小表 COUNT，不碰事实数据行，故可在巡检内零成本调用。
func (r *PartitionRepo) RoutingFactPartitionStats(ctx context.Context) (RoutingFactStats, error) {
	var out RoutingFactStats
	for _, table := range []string{"routing_quality_fact", "routing_flow_fact"} {
		names, err := r.tablePartitionNames(ctx, table)
		if err != nil {
			return RoutingFactStats{}, fmt.Errorf("list %s partitions: %w", table, err)
		}
		count := 0
		var oldest time.Time
		for _, name := range names {
			d, ok := tablePartitionDate(table, name)
			if !ok {
				out.UndatedCount++
				continue
			}
			count++
			if oldest.IsZero() || d.Before(oldest) {
				oldest = d
			}
		}
		out.Count += count
		if !oldest.IsZero() && (out.Oldest.IsZero() || oldest.Before(out.Oldest)) {
			out.Oldest = oldest
		}
		if table == "routing_quality_fact" {
			out.QualityCount = count
			out.QualityOldest = oldest
		} else {
			out.FlowCount = count
			out.FlowOldest = oldest
		}
	}
	n, err := r.CountRoutingFlowSnapshotState(ctx)
	if err != nil {
		return RoutingFactStats{}, fmt.Errorf("count routing snapshot state: %w", err)
	}
	out.SnapshotRows = n
	return out, nil
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
	IdentityVersion   int16
	RouteClassID      domain.RouteClassIDVal
	TerminalMinute    time.Time
	Ordinal           int16
	Lane              string
	AccountID         int64
	PreviousAccountID *int64
	PreviousOutcome   string
	TransitionReason  string
	Outcome           string
	IsTerminal        bool
	// Generation 是单事件代际（写面输入口径）：同边多代际输入由
	// UpsertFlowSnapshot 按新键折叠为 min_generation 后落库。
	Generation       int64
	InstanceSrc      string
	AbsoluteSequence int64
	ChainCount       int64
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

// advisoryLockTx 在事务内取分钟级 pg_advisory_xact_lock（quality upsert 用）。
func advisoryLockTx(ctx context.Context, drv *txDriver, parts ...string) error {
	var res sql.Result
	return drv.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{advisoryLockKey(parts...)}, &res)
}

// qualityUpsertSQL 生成 quality 域的逐行绝对量 upsert：INSERT 列清单与行级
// 序号守卫（WHERE EXCLUDED.absolute_sequence > <table>.absolute_sequence）逐字
// 相同，仅表名与 ON CONFLICT 目标（列序随各表唯一索引）不同。
//
// quality 的写入形状在本阶段保持不变——「逐行绝对量 upsert + 行级序号守卫」是
// 部分落库（sync.go 按 pgMaxRows/pgMaxBytes/pgMaxDuration 分批，失败时保留未落库
// 尾部）下的正确形状；改成「DELETE 本分片 + INSERT 本批」会删掉同分钟不在本批的
// 键，而那些键的 delta 已折进 committed、下一轮不再发 → 永久丢失（spec §3.1）。
func qualityUpsertSQL(table, conflictCols string) string {
	return `INSERT INTO ` + table + ` (route_class_id, quality_class_id, candidate_fingerprint, instance_src, bucket_minute, absolute_sequence, attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network, ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22, now())
	ON CONFLICT (` + conflictCols + `) DO UPDATE SET
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
	WHERE EXCLUDED.absolute_sequence > ` + table + `.absolute_sequence`
}

// routingQualityFactUpsertSQL 是质量域唯一写入缝（身份键列序同
// routing_quality_fact_uniq）。
var routingQualityFactUpsertSQL = qualityUpsertSQL("routing_quality_fact", "route_class_id, candidate_fingerprint, bucket_minute, instance_src, quality_class_id")

// UpsertQualityRow 逐行写入质量事实表（S1）：累计绝对量 upsert + 行级序号
// 守卫（WHERE EXCLUDED.absolute_sequence > …），旧序号静默 no-op（幂等重放）。
// 无下游重算，故不再置脏位；事实表就是唯一质量存储。
func (r *PartitionRepo) UpsertQualityRow(ctx context.Context, row RoutingQualityRow) error {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	bucket := row.BucketMinute.UTC().Truncate(time.Minute)
	if err := advisoryLockTx(ctx, drv, "quality", fmt.Sprintf("%d", row.IdentityVersion), bucket.Format(time.RFC3339), row.InstanceSrc); err != nil {
		return err
	}
	if row.TTFTHist == nil {
		row.TTFTHist = []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	args := []any{row.RouteClassID[:], row.QualityClassID[:], row.CandidateFingerprint[:], row.InstanceSrc, bucket, row.AbsoluteSequence, row.Attempts, row.Successes, row.Count429, row.CountOrdinary4xx, row.Count5xx, row.CountNetwork, row.TTFTN, row.TTFTSumLogQ32, row.TTFTSumSqLogQ32, row.TTFTHist, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreateTokens, row.Calls, row.Images}
	var res sql.Result
	if err := drv.Exec(ctx, routingQualityFactUpsertSQL, args, &res); err != nil {
		return err
	}
	return tx.Commit()
}

// flowEdgeKey 是合并层分片内边键（唯一索引口径：previous_account_id
// 可空，NULLS NOT DISTINCT 由 DDL 承担，此处仅做 Go 侧分组）。
type flowEdgeKey struct {
	routeClassID      domain.RouteClassIDVal
	ordinal           int16
	lane              string
	accountID         int64
	previousAccountID int64
	hasPrev           bool
	previousOutcome   string
	transitionReason  string
	outcome           string
	isTerminal        bool
}

// foldFlowRows 把同分片多代际输入按新键折叠：chain_count 求和，
// min_generation 取最小。candidate_fingerprint 换值不增行（非身份）。
func foldFlowRows(rows []RoutingFlowRow) []RoutingFlowRow {
	type acc struct {
		row RoutingFlowRow
		min int64
	}
	m := make(map[flowEdgeKey]*acc, len(rows))
	order := make([]flowEdgeKey, 0, len(rows))
	for _, row := range rows {
		k := flowEdgeKey{
			routeClassID:     row.RouteClassID,
			ordinal:          row.Ordinal,
			lane:             row.Lane,
			accountID:        row.AccountID,
			previousOutcome:  row.PreviousOutcome,
			transitionReason: row.TransitionReason,
			outcome:          row.Outcome,
			isTerminal:       row.IsTerminal,
		}
		if row.PreviousAccountID != nil {
			k.previousAccountID = *row.PreviousAccountID
			k.hasPrev = true
		}
		a, ok := m[k]
		if !ok {
			cp := row
			a = &acc{row: cp, min: row.Generation}
			m[k] = a
			order = append(order, k)
			continue
		}
		a.row.ChainCount += row.ChainCount
		if row.Generation < a.min {
			a.min = row.Generation
		}
	}
	out := make([]RoutingFlowRow, 0, len(order))
	for _, k := range order {
		a := m[k]
		a.row.Generation = a.min
		out = append(out, a.row)
	}
	return out
}

// SetRoutingObservationRetentionDays 装配观测保留天数（main 传
// cfg.Routing.ObservationRetentionDays）：写面据此拒超期快照。0/负 = 未装配，
// 守卫关闭（测试/工具路径）；生产装配缺失由 config 地板校验 + main 接线覆盖。
func (r *PartitionRepo) SetRoutingObservationRetentionDays(days int) {
	r.routingObservationDays = days
}

// DeleteRoutingFlowSnapshotStateBefore 删除早于 cutoff 的分片序号状态（保留巡检
// 日粒度调用）。snapshot_state 是交接状态，不承担历史职责：它是"本分钟本分片
// 已发布的最高序号"，一旦该分钟分区被 DROP 就再无意义（§5.4）。
func (r *PartitionRepo) DeleteRoutingFlowSnapshotStateBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var res sql.Result
	if err := r.driver.Exec(ctx, `DELETE FROM routing_flow_snapshot_state WHERE terminal_minute < $1`, []any{cutoff.UTC()}, &res); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// UpsertFlowSnapshot replaces the complete edge set for (terminal_minute, instance_src) atomically.
// Only greater absolute_sequence replaces; equal or lower does not mutate. Uses durable authority table routing_flow_snapshot_state
// so even empty snapshots advance sequence and remain authoritative independent of edge rows.
// 写合并表 routing_flow_fact（S2′）：只删己分片（minute+instance），
// 同分片多代际输入写面折叠（chain_count 求和、min_generation 取最小）。
// 无下游重算，无脏位（重算机械已整体下线）。identityVersion 仍参与 advisory 锁键
// （§3.2：常量不改变互斥，但保持锁键连续性），不再写入任何表列。
func (r *PartitionRepo) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, identityVersion int16, absoluteSequence int64, rows []RoutingFlowRow) error {
	terminalMinute = terminalMinute.UTC().Truncate(time.Minute)
	// 写面守卫（§5.4）：retention 已 DROP 早于观测截止的分钟分区并清理
	// snapshot_state；迟到的旧实例若在此之后重建该分钟，就是"删后重建"的
	// 幽灵分钟（read 面已不可见，却永久占位）。故早于截止一律拒绝。
	// 观测天数未装配（0/负，仅测试/工具路径）→ 守卫关闭。
	if r.routingObservationDays > 0 &&
		terminalMinute.Before(domain.RoutingObservationCutoff(time.Now(), r.routingObservationDays)) {
		return ErrRoutingSnapshotBeyondRetention
	}
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	drv := &txDriver{tx: tx, drv: r.driver}
	if err := advisoryLockTx(ctx, drv, "flow", fmt.Sprintf("%d", identityVersion), terminalMinute.Format(time.RFC3339), instanceSrc); err != nil {
		return err
	}
	var res sql.Result
	var curSeq sql.NullInt64
	rs := &entsql.Rows{}
	if err := drv.Query(ctx, `SELECT highest_sequence FROM routing_flow_snapshot_state WHERE terminal_minute=$1 AND instance_src=$2 FOR UPDATE`, []any{terminalMinute, instanceSrc}, rs); err != nil {
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
	if err := drv.Exec(ctx, `DELETE FROM routing_flow_fact WHERE terminal_minute=$1 AND instance_src=$2`, []any{terminalMinute, instanceSrc}, &res); err != nil {
		return err
	}
	for _, row := range foldFlowRows(rows) {
		q := `INSERT INTO routing_flow_fact (route_class_id, terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, instance_src, min_generation, chain_count, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now())`
		if err := drv.Exec(ctx, q, []any{row.RouteClassID[:], terminalMinute, row.Ordinal, row.Lane, row.AccountID, row.PreviousAccountID, row.PreviousOutcome, row.TransitionReason, row.Outcome, row.IsTerminal, instanceSrc, row.Generation, row.ChainCount}, &res); err != nil {
			return err
		}
	}
	if hasState {
		if err := drv.Exec(ctx, `UPDATE routing_flow_snapshot_state SET highest_sequence=$1, updated_at=now() WHERE terminal_minute=$2 AND instance_src=$3`, []any{absoluteSequence, terminalMinute, instanceSrc}, &res); err != nil {
			return err
		}
	} else {
		if err := drv.Exec(ctx, `INSERT INTO routing_flow_snapshot_state (terminal_minute, instance_src, highest_sequence, updated_at) VALUES ($1,$2,$3, now())`, []any{terminalMinute, instanceSrc, absoluteSequence}, &res); err != nil {
			return err
		}
	}
	return tx.Commit()
}
