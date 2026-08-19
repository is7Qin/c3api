// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/internal/ent/errlog"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/ent/usagestat"
)

// PartitionRepo 管理按日分区表（Phase 5 T4.5，用户决策 2026-08-09；err_logs/
// usage_stats 分区复用同路线，用户决策 2026-08-11）：50k 并发量级 usage_logs
// 增长 ~4.3 亿行/天——逐行 DELETE 清理不可行，按月分区单分区 26 亿行仍不可行
// → PostgreSQL 原生分区表 PARTITION BY RANGE (分区键)，每日一区（usage_logs
// ~8600 万行），保留期满直接 DROP TABLE O(1)。三表：
//   - usage_logs 计费明细（分区键 created_at；默认 30 天保留）
//   - err_logs 错误审计明细（分区键 created_at；风暴背压采样后量可控，默认
//     7 天短保留，见 usage.ErrLogRetentionDays）
//   - usage_stats 统计聚合（分区键 bucket_time——小时桶聚合 24 桶/日分区；
//     默认 180 天保留；用户裁决：PG DELETE 不释放空间，清理必须分区 DROP）
//
// 主键 (id, 分区键)（分区表硬约束：主键必须含分区键）；id 由专用序列
// {table}_id_seq 生成（ent 生成的 INSERT 不带 id 列 → 走 DEFAULT nextval，
// 与普通表 bigserial 语义一致，插入自动路由到分区键所在分区）。序列 OWNED BY
// 表列 → DROP TABLE 级联回收。
//
// Ensure/Drop 均按表参数化（表名 + 分区键 + 日期）——三表共用一套实现（单一
// 实现防漂移，P1 教训同款）；幂等语义全表通用（IF NOT EXISTS / IF EXISTS、
// 42P07/duplicate_object 容忍、并发实例安全）。
type PartitionRepo struct {
	// driver 为 raw SQL 入口（ent 无分区 DDL 能力；bootstrap/retention 均走
	// dialect.Driver.Exec/Query —— execUpdate 同构，txDriver 下同连接）。
	driver dialect.Driver
}

// 分区名 = {table}_{YYYYMMDD}（UTC 日；分区边界按 UTC 零点对齐，避免会话
// TimeZone 干扰——分区名即日期，retention 无需查元数据）。
func tablePartitionName(table string, d time.Time) string {
	return table + "_" + d.UTC().Format("20060102")
}

// tablePartitionRe 从分区名解析日期（retention DROP 边界判定）。
func tablePartitionRe(table string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(table) + `_(\d{8})$`)
}

func tablePartitionDate(table, name string) (time.Time, bool) {
	m := tablePartitionRe(table).FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// partitionedCreateDDL 分区表 DDL（由列定义事实源生成；列定义与 ent schema
// 完全一致，仅主键与 id 生成方式不同——ent migrate 主键 (id) 在分区表上不可行，
// 见 ensureTablePartitioned 注释与 migrateHookExcludesPartitioned；id 走序列
// nextval，语义同 ent bigserial）。
// partitionCol 为分区键列（usage_logs/err_logs = created_at；usage_stats =
// bucket_time——用户裁决 2026-08-11：三表统一分区机制，主键必含分区键）。
func partitionedCreateDDL(table, partitionCol string, columnDefs []string) string {
	return "CREATE TABLE " + table + " (\n\t" +
		strings.Join(columnDefs, ",\n\t") + ",\n\t" +
		"PRIMARY KEY (id, " + partitionCol + ")\n" +
		") PARTITION BY RANGE (" + partitionCol + ")"
}

// usageLogColumnDefs usage_logs 分区表列定义（单一事实源，评审 I-1 锚）：
// 建表 DDL 由本列表生成（partitionedCreateDDL）——列集合一致性由
// TestUsageLogColumnDefsMatchCreateDDL 锚定（防"向静态 DDL 加列忘加事实源"
// 漂移，含类型漂移——列定义字符串整体生成）。列定义与 ent schema 完全一致，
// 仅主键与 id 生成方式不同（见 partitionedCreateDDL 注释）。用户裁决
// （err_logs 分表）：usage_logs 瘦身去 2 留 1——status_code 与 error_message
// 列移除（错误审计由 err_logs 承载，见 errLogColumnDefs）。
var usageLogColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('usage_logs_id_seq'::regclass)`,
	`request_id varchar NOT NULL`,
	// 用户裁决（2026-08-17，S-E）：client_ip 审计列（紧随 request_id 审计聚集）——
	// 供应商头识别 + RemoteAddr 兜底（proxy.behind_cdn 门控）；NULL 语义 =
	// Optional（与 ent schema 一致，未 Set 的列不写）。
	`client_ip text NULL`,
	`group_id bigint NULL`,
	`account_id bigint NULL`,
	`template_id bigint NULL`,
	`user_id bigint NULL`,
	`key_id bigint NULL`,
	`model varchar NOT NULL DEFAULT ''`,
	`mapped_model varchar NULL`,
	`format varchar NOT NULL`,
	`error_type varchar NOT NULL DEFAULT 'none'`,
	`latency_ms bigint NOT NULL DEFAULT 0`,
	`ttft_ms bigint NULL`,
	`input_tokens bigint NOT NULL DEFAULT 0`,
	`price_input_millis bigint NULL`,
	`output_tokens bigint NOT NULL DEFAULT 0`,
	`price_output_millis bigint NULL`,
	`total_tokens bigint NOT NULL DEFAULT 0`,
	`cache_read_tokens bigint NOT NULL DEFAULT 0`,
	`price_cache_read_millis bigint NULL`,
	`cache_creation_tokens bigint NOT NULL DEFAULT 0`,
	`price_cache_creation_millis bigint NULL`,
	// 统一计费模型功能调用分量（spec 2026-08-13）：call_count 功能调用计数
	// （图片生成 = 张数/data 长/completed 事件数、search = 1；不入 TotalTokens）
	// + price_per_call_millis 按单元价快照（**毫分/单元**——search 每次/图片每张，
	// 例外于本表其余 price_*_millis 列的"毫分/1M tokens"口径——per-call 计费
	// 不走 /1e6 除法，spec §4.2 例外说明）。原图片 6 专列已删（image token 并
	// 入 input/output_tokens——TotalTokens 口径不变）。
	`call_count bigint NOT NULL DEFAULT 0`,
	`price_per_call_millis bigint NULL`,
	`cost bigint NOT NULL DEFAULT 0`,
	// raw_cost（spec 2026-08-18）：乘倍率前的原始成本（毫分）——免费组
	// cost=0 但 raw 有值（"实际消耗"可见）；历史行/缺省 = 0（fresh setup
	// 不迁移）。
	`raw_cost bigint NOT NULL DEFAULT 0`,
	`billing_tier varchar NULL`,
	`above_hit boolean NOT NULL DEFAULT false`,
	`overdraft boolean NOT NULL DEFAULT false`,
	`created_at timestamptz NOT NULL`,
}

var usageLogCreateDDL = partitionedCreateDDL("usage_logs", "created_at", usageLogColumnDefs)

// usageLogIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为
// 分区索引，子分区自动继承）。唯一索引含分区键 created_at（分区表硬约束，
// 见本文件头注释）：request_id 幂等键（方向 A 批次 1a，A-P2-3）——COMMIT
// 歧义窗口重试撞 23505 由 flusher 按成功处理（防双扣，见
// internal/billing/flusher.go isUniqueLogConflict）。索引随 bootstrap 重建
// 路径必建（全新安装恒齐，无手动补建面）。
var usageLogIndexDDLs = []string{
	`CREATE INDEX usagelog_created_at ON usage_logs (created_at)`,
	`CREATE INDEX usagelog_group_id_created_at ON usage_logs (group_id, created_at)`,
	`CREATE INDEX usagelog_account_id_created_at ON usage_logs (account_id, created_at)`,
	`CREATE INDEX usagelog_user_id_created_at ON usage_logs (user_id, created_at)`,
	`CREATE INDEX usagelog_key_id_created_at ON usage_logs (key_id, created_at)`,
	`CREATE UNIQUE INDEX usagelog_request_id_created_at ON usage_logs (request_id, created_at)`,
}

// errLogColumnDefs err_logs 分区表列定义（单一事实源，与 ent schema 完全一致，
// 锚测试 TestErrLogColumnDefsMatchCreateDDL 断言列集合一致）：错误审计瘦表——
// 无 token/价格列；status_code/error_message（usage_logs 瘦身去掉的排障列）+
// 审计归属（group/account/template/user/key）+ billing_tier（评审 I-3：tier
// reject 的 tier 维度审计保留）。
var errLogColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('err_logs_id_seq'::regclass)`,
	`request_id varchar NOT NULL`,
	// 用户裁决（2026-08-17，S-E）：client_ip 审计列（紧随 request_id）——拒绝行
	// （401 鉴权失败等）也带（guardPipeline 入口鉴权前提取）；NULL = Optional。
	`client_ip text NULL`,
	`group_id bigint NULL`,
	`account_id bigint NULL`,
	`template_id bigint NULL`,
	`user_id bigint NULL`,
	`key_id bigint NULL`,
	`model varchar NOT NULL DEFAULT ''`,
	`format varchar NOT NULL`,
	`status_code integer NOT NULL DEFAULT 0`,
	`error_type varchar NOT NULL DEFAULT 'none'`,
	`error_message varchar NULL`,
	`latency_ms bigint NOT NULL DEFAULT 0`,
	`billing_tier varchar NULL`,
	`created_at timestamptz NOT NULL`,
}

var errLogCreateDDL = partitionedCreateDDL("err_logs", "created_at", errLogColumnDefs)

// errLogIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为分区
// 索引，子分区自动继承）：created_at 时间窗口查询/清理 + (group_id/user_id,
// created_at) 查询面（架构审查 S1——/err_logs 按用户/组过滤）。
var errLogIndexDDLs = []string{
	`CREATE INDEX errlog_created_at ON err_logs (created_at)`,
	`CREATE INDEX errlog_group_id_created_at ON err_logs (group_id, created_at)`,
	`CREATE INDEX errlog_user_id_created_at ON err_logs (user_id, created_at)`,
}

// usageStatsColumnDefs usage_stats 分区表列定义（单一事实源；spec 2026-08-14
// 离线聚合化重建：total_latency_ms 删除 + call_count（按次调用）+ TTFT 四列
// （ttft_total_ms/ttft_count/ttft_max_ms/ttft_hist bigint[10] 直方图）。列定义与
// ent schema 一致**除 ttft_hist**——PG bigint[] 数组列 ent 无类型（field.Ints 等
// 是 JSON 语义，无法扫描 PG 数组），数组列 carve-out 不进 ent schema（评审
// P1-1），ScanStats 改 pgx 直查扫描 []int64。分区键 bucket_time（小时桶聚合
// 24 桶/区）。id 走 usage_stats_id_seq（ent bigserial 同款语义——INSERT 不带 id
// 列走 DEFAULT nextval；该表经 bootstrap 建表（表不存在则建，全新安装唯一路
// 径），零迁移逻辑。
var usageStatsColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('usage_stats_id_seq'::regclass)`,
	`bucket_time timestamptz NOT NULL`,
	`group_id bigint NOT NULL DEFAULT 0`,
	`account_id bigint NOT NULL DEFAULT 0`,
	`template_id bigint NOT NULL DEFAULT 0`,
	`user_id bigint NOT NULL DEFAULT 0`,
	`model varchar NOT NULL DEFAULT ''`,
	`is_error boolean NOT NULL DEFAULT false`,
	`request_count bigint NOT NULL DEFAULT 0`,
	`error_count bigint NOT NULL DEFAULT 0`,
	`input_tokens bigint NOT NULL DEFAULT 0`,
	`output_tokens bigint NOT NULL DEFAULT 0`,
	`total_tokens bigint NOT NULL DEFAULT 0`,
	`cache_read_tokens bigint NOT NULL DEFAULT 0`,
	`cache_creation_tokens bigint NOT NULL DEFAULT 0`,
	`cost bigint NOT NULL DEFAULT 0`,
	// raw_cost（spec 2026-08-19）：乘倍率前的原始成本（毫分）——离线聚合
	// SUM(raw_cost)（usage_logs 同源口径）；历史桶/缺省 = 0（fresh setup
	// 不迁移，重算即填）。
	`raw_cost bigint NOT NULL DEFAULT 0`,
	// 按次调用（用户裁决 2026-08-14）：图片生成 = 张数、search = 1；离线聚合
	// sum(call_count) 直取（usage_logs 明细已有 CallCount——图片 6 专列删后的
	// 统一计费模型分量，spec 2026-08-13）。
	`call_count bigint NOT NULL DEFAULT 0`,
	`ttft_total_ms bigint NOT NULL DEFAULT 0`,
	`ttft_count bigint NOT NULL DEFAULT 0`,
	`ttft_max_ms bigint NOT NULL DEFAULT 0`,
	// TTFT 直方图 10 档（spec 2026-08-14）：[0,50) [50,100) [100,200) [200,400)
	// [400,800) [800,1600) [1600,3200) [3200,6400) [6400,12800) [12800,∞)——
	// SQL 侧 count(*) FILTER 逐档计数（PG 原生，零自定义聚合）；查询侧 Go
	// 逐元素合并 + 桶内线性插值（顶桶回落下界 12800，见 stat_repo.go）。
	`ttft_hist bigint[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0,0,0}'`,
	`updated_at timestamptz NOT NULL`,
}

var usageStatsCreateDDL = partitionedCreateDDL("usage_stats", "bucket_time", usageStatsColumnDefs)

// usageStatsIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为分区
// 索引，子分区自动继承）。唯一索引含分区键 bucket_time（分区表硬约束：唯一
// 索引必须含分区键；顺带即 Upsert 的 ON CONFLICT 目标列序——batched COPY
// 两阶段 merge 在分区表上行为不变）。bucket_time 独立索引为纯写放大无查询受
// 益（(user_id, bucket_time) 复合索引已覆盖 user_id 前缀查询面），F3-1 已删
// （p2-18 P2-B 建议删 + 用户裁决 2026-08-13 取删）。
var usageStatsIndexDDLs = []string{
	`CREATE UNIQUE INDEX usagestat_bucket_time_group_id_account_id_template_id_user_id_model_is_error ON usage_stats (bucket_time, group_id, account_id, template_id, user_id, model, is_error)`,
	`CREATE INDEX usagestat_user_id_bucket_time ON usage_stats (user_id, bucket_time)`,
}

// IsTablePartitioned 查 pg_partitioned_table（pg_class.relkind='p'）判断指定
// 表是否已是分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsTablePartitioned(ctx context.Context, table string) (bool, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relname = $1 AND c.relkind = 'p'`, []any{table}, rows); err != nil {
		return false, err
	}
	defer rows.Close()
	n, err := entsql.ScanInt(rows)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// IsUsageLogPartitioned usage_logs 是否分区表（bootstrap 幂等判定；既有 API）。
func (r *PartitionRepo) IsUsageLogPartitioned(ctx context.Context) (bool, error) {
	return r.IsTablePartitioned(ctx, "usage_logs")
}

// IsErrLogPartitioned err_logs 是否分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsErrLogPartitioned(ctx context.Context) (bool, error) {
	return r.IsTablePartitioned(ctx, "err_logs")
}

// IsUsageStatsPartitioned usage_stats 是否分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsUsageStatsPartitioned(ctx context.Context) (bool, error) {
	return r.IsTablePartitioned(ctx, "usage_stats")
}

// partitionExists 分区表是否存在（幂等预建判定；pgx 下 to_regclass 错误处理
// 复杂，直接查 pg_class，任意 relkind 均可——分区子表 relkind='r'）。
func (r *PartitionRepo) partitionExists(ctx context.Context, name string) (bool, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relname = $1`, []any{name}, rows); err != nil {
		return false, err
	}
	defer rows.Close()
	n, err := entsql.ScanInt(rows)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// execDDL 执行无参数 DDL（bootstrap/retention；v 形参 *sql.Result 与
// execUpdate 同构）。
func (r *PartitionRepo) execDDL(ctx context.Context, query string) error {
	var res sql.Result
	return r.driver.Exec(ctx, query, []any{}, &res)
}

// isDuplicateObject 判断"对象已存在"类竞态错误（多实例并发 bootstrap/预建
// 分区撞名容忍；ent Conn.Exec 原样透传 pgconn 错误，无需解包）：
//   - 42P07 / 42710 duplicate_object：无 IF NOT EXISTS 的 CREATE（分区表/索引/
//     日分区）撞已存在对象——同一次撞名 PG 按时序报两种码：并发 CREATE TABLE
//     双方都通过"是否已分区"判定时，败者类型名预检落在胜者提交之后 → 撞胜者
//     隐式复合类型 → "type X already exists" 42710；预检落在胜者提交之前 →
//     关系插入撞锁后 42P07（实测并发 bootstrap 两种码均现，TestUsageLog
//     PartitionConcurrentBootstrapPG -race 复现）；
//   - 23505 unique_violation：IF NOT EXISTS 的 CREATE SEQUENCE 并发创建时
//     "检查-插入"在 pg_class 唯一索引上竞态（PG 已知行为，实测出现）——
//     仅在 bootstrap 的 CREATE 步骤使用本判定，23505 只能来自并发建对象。
func isDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42P07" || pgErr.Code == "42710" || pgErr.Code == "23505"
}

// isMissingObject 判断"目标对象不存在"竞态错误（42P01 undefined_table）：
// 并发 bootstrap 的 **stale-DROP 窗口**专用（评审 I-1 已接受的窗口，见
// ensureTablePartitioned 注释）——实例基于过期的"未分区"判定执行 DROP TABLE
// IF EXISTS，可能误删对方刚建的表（含 OWNED BY 级联的序列）；被删侧后续步骤
// 短暂撞 42P01：OWNED BY/索引/分区引用缺失的表、CREATE TABLE 引用被级联
// 删掉的序列。容忍后由"最后执行 DROP 的实例"补建收敛（其 CREATE 必然成功且
// 无后续 DROP，表最终存在，所有权/索引/分区随之落位）。单实例下 42P01 属配
// 置错误——容忍后由后续 INSERT 显式失败兜底（与撞名容忍同哲学：不阻塞启动、
// 下游响亮失败）。
func isMissingObject(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42P01"
}

// isBootstrapRaceError 并发 bootstrap 竞态判定（撞名 isDuplicateObject + 缺失
// isMissingObject）：任何"另一实例在途"的 DDL 结果（对象已存在或暂缺）均视为
// 成功继续，双方幂等收敛（最终状态一致，见 ensureTablePartitioned 注释）。
func isBootstrapRaceError(err error) bool {
	return isDuplicateObject(err) || isMissingObject(err)
}

// execDDLTolerateRace 执行 DDL；并发 bootstrap 竞态类错误（撞名 42P07/42710/
// 23505 + 缺失 42P01，见 isBootstrapRaceError）视为成功（另一实例在途，多实
// 例语义见 ensureTablePartitioned），其余错误原样返回。
func (r *PartitionRepo) execDDLTolerateRace(ctx context.Context, query string) error {
	if err := r.execDDL(ctx, query); err != nil {
		if isBootstrapRaceError(err) {
			return nil
		}
		return err
	}
	return nil
}

// ensureTablePartitioned bootstrap（幂等，main 装配在 ent migrate 之后调用）：
// bootstrap 即建表路径（全新安装唯一）——ent migrate 经钩子整表过滤三张分区
// 表（migrateHookExcludesPartitioned），分区表仅由本函数创建：未分区 → DROP
// TABLE（IF EXISTS 恒 no-op）+ 重建分区表/序列/索引 + 预建分区（表结构终态由
// 列事实源定义）；已是分区表 → 仅确保 当日→明日 分区存在后返回。
//
// 多实例语义（评审 I-1）：两实例同时启动时，"是否已分区"判定与 CREATE 之间
// 另一实例可能已建对象——所有 CREATE 步骤（分区表/索引/日分区）对撞名类错误
// （42P07/42710/23505）容忍后继续，双方幂等收敛。DROP 为 IF EXISTS 不报错；
// 理论窗口下并发实例的 DROP 误删对方刚建的分区表时（stale-DROP 窗口），被删
// 侧 OWNED BY/索引/分区短暂撞 42P01——同样容忍（isBootstrapRaceError 完整覆
// 盖），由"最后执行 DROP 的实例"补建，最终状态一致（对象集合收敛）。
func (r *PartitionRepo) ensureTablePartitioned(ctx context.Context, table, partitionCol string, columnDefs, indexDDLs []string, now time.Time) error {
	parted, err := r.IsTablePartitioned(ctx, table)
	if err != nil {
		return err
	}
	if !parted {
		if err := r.execDDL(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("drop plain %s: %w", table, err)
		}
		// 序列独立于表创建（CREATE TABLE 的 DEFAULT 需先存在）；OWNED BY 使
		// DROP TABLE 级联回收（serial 同款生命周期）。IF NOT EXISTS 并发下
		// 仍可能 23505（catalog 唯一索引竞态，实测），同样容忍。全步骤走
		// execDDLTolerateRace（撞名 + stale-DROP 窗口缺失 42P01——OWNED BY 目
		// 标表/索引父表/级联删的序列都可能短暂缺失，见 isMissingObject 注释）。
		seqName := table + "_id_seq"
		if err := r.execDDLTolerateRace(ctx, `CREATE SEQUENCE IF NOT EXISTS `+seqName); err != nil {
			return fmt.Errorf("create %s: %w", seqName, err)
		}
		if err := r.execDDLTolerateRace(ctx, partitionedCreateDDL(table, partitionCol, columnDefs)); err != nil {
			return fmt.Errorf("create partitioned %s: %w", table, err)
		}
		if err := r.execDDLTolerateRace(ctx, `ALTER SEQUENCE `+seqName+` OWNED BY `+table+`.id`); err != nil {
			return fmt.Errorf("own sequence: %w", err)
		}
		for _, idx := range indexDDLs {
			if err := r.execDDLTolerateRace(ctx, idx); err != nil {
				return fmt.Errorf("create %s index: %w", table, err)
			}
		}
	}
	return r.EnsureTablePartitions(ctx, table, now, now.AddDate(0, 0, 1))
}

// EnsureUsageLogPartitioned usage_logs 分区 bootstrap（既有 API，见
// ensureTablePartitioned；分区键 created_at）。
func (r *PartitionRepo) EnsureUsageLogPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "usage_logs", "created_at", usageLogColumnDefs, usageLogIndexDDLs, now)
}

// EnsureErrLogPartitioned err_logs 分区 bootstrap（同路线复用：独立列事实源 +
// 独立序列 err_logs_id_seq + 独立保留期；分区键 created_at）。
func (r *PartitionRepo) EnsureErrLogPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "err_logs", "created_at", errLogColumnDefs, errLogIndexDDLs, now)
}

// statsAggWatermarkDDL 离线聚合 watermark 单行表（spec 2026-08-14：settings
// key-value 形态——单行恒 id=1，CHECK 约束钉死单行；worker 每周期读聚合位置、
// 推进与 DELETE+INSERT 同事务（崩溃回滚 → 游标不动 → 重算恢复不双计）。
// 全新库初始化 = now − 滞后（防首跑扫全史 + DELETE 撞 retention 已 DROP 分区），
// ON CONFLICT DO NOTHING 容忍多实例并发初始化。单行表无分区需求（恒 1 行，
// 不参与保留清理）。IF NOT EXISTS 幂等（bootstrap 重复执行无副作用）。
var statsAggWatermarkDDL = `CREATE TABLE IF NOT EXISTS stats_agg_watermark (
	id bigint NOT NULL,
	watermark timestamptz NOT NULL,
	PRIMARY KEY (id),
	CONSTRAINT stats_agg_watermark_single CHECK (id = 1)
)`

// EnsureUsageStatsPartitioned usage_stats 分区 bootstrap（用户裁决 2026-08-11：
// 三表统一分区机制；分区键 bucket_time——小时桶聚合 24 桶/日分区；建表语义同
// ensureTablePartitioned）。同步骤建 stats_agg_watermark 单行表（离线聚合
// worker 的 watermark 存储；撞名类竞态容忍——多实例并发 bootstrap 收敛语义同
// ensureTablePartitioned）。
func (r *PartitionRepo) EnsureUsageStatsPartitioned(ctx context.Context, now time.Time) error {
	if err := r.execDDLTolerateRace(ctx, statsAggWatermarkDDL); err != nil {
		return fmt.Errorf("create stats_agg_watermark: %w", err)
	}
	return r.ensureTablePartitioned(ctx, "usage_stats", "bucket_time", usageStatsColumnDefs, usageStatsIndexDDLs, now)
}

// EnsureTablePartitions 确保 [trunc(now), trunc(until)] 每日分区存在
// （幂等：已存在跳过；until 早于 now → 仅 now 当日）。bootstrap 与 retention
// worker 共用——防日界竞态：分区未建时插入跨日 row 会整体失败（PG 对分区表
// 无自动建分区），必须预留未来分区。start/end 边界统一由调用方传入的 now
// 推导（评审 I-2：不内部取 time.Now()，测试可注入任意时钟；worker 每轮
// 现取 now 传入）。
func (r *PartitionRepo) EnsureTablePartitions(ctx context.Context, table string, now, until time.Time) error {
	start := now.UTC().Truncate(24 * time.Hour)
	end := until.UTC().Truncate(24 * time.Hour)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		name := tablePartitionName(table, d)
		ok, err := r.partitionExists(ctx, name)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		from := d.Format("2006-01-02 15:04:05-07")
		to := d.AddDate(0, 0, 1).Format("2006-01-02 15:04:05-07")
		if err := r.execDDL(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`, name, table, from, to)); err != nil {
			if isMissingObject(err) {
				// stale-DROP 窗口父表暂缺：跳过本日——父表由后建者补建，其
				// 同参数分区预建覆盖本日（收敛语义见 isMissingObject；单实例
				// 配置级缺失 → 后续 INSERT 显式失败兜底）。
				continue
			}
			if isDuplicateObject(err) {
				// 并发实例已建该分区：重查确认后继续（多实例语义同上）
				ok2, err2 := r.partitionExists(ctx, name)
				if err2 != nil {
					return err2
				}
				if ok2 {
					continue
				}
				return fmt.Errorf("create partition %s: %w", name, err)
			}
			return fmt.Errorf("create partition %s: %w", name, err)
		}
	}
	return nil
}

// EnsureUsageLogPartitions usage_logs 每日分区预建（既有 API）。
func (r *PartitionRepo) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	return r.EnsureTablePartitions(ctx, "usage_logs", now, until)
}

// EnsureErrLogPartitions err_logs 每日分区预建（retention worker 共用）。
func (r *PartitionRepo) EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error {
	return r.EnsureTablePartitions(ctx, "err_logs", now, until)
}

// EnsureUsageStatsPartitions usage_stats 每日分区预建（retention worker 共用；
// 分区键 bucket_time——日界分区从 now 当日零点起）。
func (r *PartitionRepo) EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error {
	return r.EnsureTablePartitions(ctx, "usage_stats", now, until)
}

// DropTablePartitionsBefore DROP 指定表分区下界日期早于 cutoff 的分区（O(1)，
// 按分区名日期判定，无需查元数据）；返回删除个数。保留 >= cutoff 的分区。
func (r *PartitionRepo) DropTablePartitionsBefore(ctx context.Context, table string, cutoff time.Time) (int, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace n ON n.oid = c.relnamespace WHERE p.relname = $1 AND n.nspname = current_schema()`, []any{table}, rows); err != nil {
		return 0, err
	}
	names := []string{}
	if err := entsql.ScanSlice(rows, &names); err != nil {
		return 0, err
	}
	cut := cutoff.UTC().Truncate(24 * time.Hour)
	dropped := 0
	for _, name := range names {
		d, ok := tablePartitionDate(table, name)
		if !ok {
			continue
		}
		if d.Before(cut) {
			if err := r.execDDL(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
				return dropped, fmt.Errorf("drop partition %s: %w", name, err)
			}
			dropped++
		}
	}
	return dropped, nil
}

// DropUsageLogPartitionsBefore usage_logs DROP 分区下界早于 cutoff 的分区
// （既有 API；O(1)）。保留 >= cutoff 的分区。
func (r *PartitionRepo) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "usage_logs", cutoff)
}

// DropErrLogPartitionsBefore err_logs DROP 分区下界早于 cutoff 的分区（独立
// 保留期：cutoff 由 usage.ErrLogRetentionDays 推导，两表各自调度）。
func (r *PartitionRepo) DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "err_logs", cutoff)
}

// DropUsageStatsPartitionsBefore usage_stats DROP 分区下界早于 cutoff 的分区
// （用户裁决 2026-08-11：PG DELETE 不释放空间，180 天保留清理必须分区 DROP
// O(1)——替代逐行 DELETE 方案；cutoff 由 usage.StatsRetentionDays 推导）。
func (r *PartitionRepo) DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "usage_stats", cutoff)
}

// redemptionUsesDeleteBatchLimit redemption_uses 每轮批删上限（F3-2 批删有界：
// 普通表无分区可 DROP，不能对齐分区表 O(1) DROP 形态——有界 DELETE 防长事务
// 持锁；低频表单轮即清，超大批多轮收敛）。
const redemptionUsesDeleteBatchLimit = 5000

// DeleteRedemptionUsesBefore redemption_uses 有界批删（F3-2，retention worker
// 周期任务调用；TTL 定死 90 天，cutoff = now - 90 天由调用方推导）：
//   - 普通表无分区可 DROP → 走 DELETE 批删路径（与三张分区表 O(1) DROP 并存，
//     均为 retention worker 周期面内的清理手段）；
//   - 每轮至多删 redemptionUsesDeleteBatchLimit 行（子查询 LIMIT 有界；DELETE
//     按 id 升序取超窗行——收敛确定，无游标/页码，多轮各自重新取最小 id）；
//   - 返回本轮实际删除行数（0 = 已收敛，幂等）。
func (r *PartitionRepo) DeleteRedemptionUsesBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var res sql.Result
	query := `DELETE FROM redemption_uses WHERE id IN (SELECT id FROM redemption_uses WHERE created_at < $1 ORDER BY id LIMIT ` + strconv.Itoa(redemptionUsesDeleteBatchLimit) + `)`
	if err := r.driver.Exec(ctx, query, []any{cutoff}, &res); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// migrateHookExcludesPartitioned 让 ent migrate 跳过分区表（usage_logs +
// err_logs + usage_stats）——分区表 DDL 由 ensureTablePartitioned 独占管理。
// 真实 PG 实测
// 结论（2026-08-09，ent v0.14.6 + atlas v0.36.2 + PostgreSQL 18）：atlas 能
// 识别已存在的分区表（分区键属性），与 ent schema 的普通表定义 diff 时在规划
// 期直接报错 "sql/schema: partition key cannot be dropped from ..."——即任何
// "普通表 → 分区表"的 diff 都不可行，ent migrate 对分区表必然失败，且无
// migrate 选项可容忍（ent 无禁用主键/分区键 diff 的选项）。故用 schema.WithHooks
// 在 Create 前过滤这些表，ent 永不 diff/DDL 分区表；表的存在性/列结构/索引
// 完全由 bootstrap 维护（与 ent schema 列定义一致，见 partitionedCreateDDL）。
func migrateHookExcludesPartitioned() schema.MigrateOption {
	return schema.WithHooks(func(next schema.Creator) schema.Creator {
		return schema.CreateFunc(func(ctx context.Context, tables ...*schema.Table) error {
			kept := make([]*schema.Table, 0, len(tables))
			for _, t := range tables {
				if t.Name != usagelog.Table && t.Name != errlog.Table && t.Name != usagestat.Table {
					kept = append(kept, t)
				}
			}
			return next.Create(ctx, kept...)
		})
	})
}
