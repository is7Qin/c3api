// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/template"
)

// StatRepo 统计仓库（spec 2026-08-14 离线聚合化 + spec 2026-08-23 v2 重构）：
// 离线聚合写入面（LoadAggRange/AggregateRange——cube 两源 + 实体六查询）、
// 读取面（stat_query_repo.go StatsTrend 族）与 overview 聚合面（SummarizeStats/
// ScanStatsDays）全部经 pgx 原生池直查直写——ent client 仅用于资源计数等非
// 统计面（usage_stats 含 bigint[] 数组列，ent 无类型，ent carve-out 评审 P1-1）。
// pool 由 NewWithPG 构造注入（生产 main.go 注入 OpenPG 池；与 ent driver 同
// DSN 共享连接上限）。usage_stats / usage_entity_stats 均为分区表（用户裁决
// 2026-08-11：PG DELETE 不释放空间，保留清理必须 DROP 分区 O(1)）——清理由
// retention worker 经 PartitionRepo 执行；两表只由离线聚合 worker 写入
// （DELETE+INSERT 覆盖语义，无双写者、无 merge 累加——issue #8 教训）。
type StatRepo struct {
	client *ent.Client
	pool   *pgxpool.Pool
}

// —— 离线聚合写入面（spec 2026-08-14 §3；单一写者：usage_stats 只由聚合 worker 写入） ——

// statsAggBatchSize 单条批量 INSERT 的桶数（18 列 × 500 ≈ 9k 参数，PG 参数
// 上限 65535 安全；沿用 statBatchSize 纪律——离线聚合每 5 分钟一轮，冷路径）。
const statsAggBatchSize = 500

// statsAggLockKey 聚合 worker 会话级 advisory lock 键（固定魔数——多实例以
// 同一键互斥，仅一个实例执行聚合；键值任意恒定即可）。
const statsAggLockKey int64 = 0x53746174 // "Stat"

// ttftHistBounds TTFT 直方图 10 档下界（spec 2026-08-14 §1）：[0,50) [50,100)
// [100,200) [200,400) [400,800) [800,1600) [1600,3200) [3200,6400) [6400,12800)
// [12800,∞)。SQL 侧逐档 count(*) FILTER 条件（aggHistExpr）与 DDL DEFAULT 均
// 与档位钉死同步；本表仅供查询侧插值（唯一事实源 = SQL FILTER 条件）。
var ttftHistBounds = []int64{0, 50, 100, 200, 400, 800, 1600, 3200, 6400, 12800}

// ttftZeroHist 全零直方图（错误桶 TTFT 恒 0 的 INSERT 参数形态；len = 10）。
var ttftZeroHist = make([]int64, len(ttftHistBounds))

// mergeHist 直方图逐元素合并（array_agg 带回 [][]int64，Go 侧合并——加法交换
// 序无关；行数 ≤ 30 天 × 维度，数组 ≤ 24×10 元素/行，O(万级)，不违反"不拉全行
// 聚合"纪律）。
func mergeHist(dst, src []int64) {
	for i := range src {
		if i >= len(dst) {
			break
		}
		dst[i] += src[i]
	}
}

// TTFTPercentileMS 直方图桶内线性插值（spec 2026-08-14 §1 公式钉死；rewrite
// spec 2026-08-14 插值复用契约：/stats + /api/user/stats 端点经本导出函数共用——
// overview 查询与端点插值同一实现，无第二份逻辑）：
// low + (rank − cumBelow) / bucketCount × width；rank = ceil(p × N)（nearest-
// rank）；落在顶桶 [12800, ∞) → 返回下界 12800（**顶桶不可插值**——无上界，
// 注释标注；返回下界是保守下限口径）。无样本（N = 0）→ 0。
func TTFTPercentileMS(hist []int64, n int64, p float64) int64 {
	if n <= 0 {
		return 0
	}
	rank := int64(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	var cum int64
	for i, cnt := range hist {
		if i >= len(ttftHistBounds) {
			break
		}
		if cnt == 0 {
			continue
		}
		if cum+cnt >= rank {
			low := ttftHistBounds[i]
			if i == len(ttftHistBounds)-1 {
				return low // 顶桶回落（无上界不可插值）
			}
			width := ttftHistBounds[i+1] - low
			frac := float64(rank-cum) / float64(cnt)
			return low + int64(frac*float64(width))
		}
		cum += cnt
	}
	return ttftHistBounds[len(ttftHistBounds)-1] // 防御：hist 为空/全零 → 顶桶下界
}

// TTFTAvgMS TTFT 均值（查询侧 Go 除——SQL 只 sum/count/max，spec P3 措辞）；
// 无样本 → 0。除法后 math.Round 收敛整数毫秒（用户裁决 2026-08-14：裸浮点
// 直出——dashboard 显示 322.96317829457365；毫秒语义整数，契约 number 不变）。
func (s *StatSummary) TTFTAvgMS() float64 {
	if s.TTFTCount <= 0 {
		return 0
	}
	return math.Round(float64(s.TTFTTotalMS) / float64(s.TTFTCount))
}

// TTFTPercentileMS p 分位（nearest-rank + 桶内线性插值；无样本 → 0）。
func (s *StatSummary) TTFTPercentileMS(p float64) int64 {
	return TTFTPercentileMS(s.TTFTHist, s.TTFTCount, p)
}

// TTFTAvgMS StatDayAgg 版（同上；日桶无样本 → 0；同款 math.Round 收敛）。
func (d *StatDayAgg) TTFTAvgMS() float64 {
	if d.TTFTCount <= 0 {
		return 0
	}
	return math.Round(float64(d.TTFTTotalMS) / float64(d.TTFTCount))
}

// TTFTPercentileMS StatDayAgg 版（同上）。
func (d *StatDayAgg) TTFTPercentileMS(p float64) int64 {
	return TTFTPercentileMS(d.TTFTHist, d.TTFTCount, p)
}

// aggDimCols cube 两查询共享维度列前缀（v2 三维：hour × group × model；重算
// 范围 [from,to) 占位 $1/$2）。usage_logs/err_logs 的 group_id 可空列 COALESCE
// 归零——GROUP BY 位置引用与 INSERT NOT NULL 一致（无主行归 0 组保留——cube
// 是平台总卷；与实体卷积表的 IS NOT NULL 丢弃语义有意不对称，见
// stat_entity_agg.go）；model 列两表均 NOT NULL，COALESCE 防御性保留（spec
// SQL 形状钉死）。小时桶 UTC 墙钟截断——会话 TimeZone 无关，与 usage_stats
// 分区键对齐。
var aggDimCols = `date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       COALESCE(group_id, 0), COALESCE(model, '')`

// aggHistExpr 直方图 10 档 count(*) FILTER（PG 原生，零自定义聚合；档位边界
// 与 ttftHistBounds/DDL DEFAULT 钉死同步）。ttft_ms 可空（非流式/失败路径
// NULL），FILTER 恒 false 即不计档。
var aggHistExpr = `ARRAY[count(*) FILTER (WHERE ttft_ms < 50),
       count(*) FILTER (WHERE ttft_ms >= 50 AND ttft_ms < 100),
       count(*) FILTER (WHERE ttft_ms >= 100 AND ttft_ms < 200),
       count(*) FILTER (WHERE ttft_ms >= 200 AND ttft_ms < 400),
       count(*) FILTER (WHERE ttft_ms >= 400 AND ttft_ms < 800),
       count(*) FILTER (WHERE ttft_ms >= 800 AND ttft_ms < 1600),
       count(*) FILTER (WHERE ttft_ms >= 1600 AND ttft_ms < 3200),
       count(*) FILTER (WHERE ttft_ms >= 3200 AND ttft_ms < 6400),
       count(*) FILTER (WHERE ttft_ms >= 6400 AND ttft_ms < 12800),
       count(*) FILTER (WHERE ttft_ms >= 12800)]`

// aggUsageSQL usage_logs 单扫描 → cube 桶（v2：is_error 出键后 success/abort
// 双查询合一；spec 2026-08-23 §3.1）。
//
// 语义等价性论证（旧双查询 vs 本单扫描，逐字段）：旧口径下 success 行
// （error_type='none'）成桶 rc=count(*)、ec=0；abort 行（error_type='abort'）
// 成桶 rc=ec=count(*)。本单扫描 WHERE error_type IN ('none','abort') 后维度
// 不再含 is_error——同维度 none 行与 abort 行合并为一桶：
//   - rc = count(*) = rc_none + rc_abort（两旧行集不相交，恒等）；
//   - ec = count(*) FILTER (WHERE error_type <> 'none') = rc_abort（'abort' 是
//     该行集中唯一非 none 值）= 旧 abort 桶 ec + 旧 success 桶 ec(=0)，恒等；
//   - 测量列（tokens×5/cost/raw_cost/call_count/ttft sum/count/max/hist 逐档
//     FILTER 计数）对不相交行集可加——合并桶 = 两旧桶逐列相加。
//
// err_logs 纯错误桶补充照抄旧 aggErrLogSQL 口径（aggErrLogSQL）。
var aggUsageSQL = `SELECT ` + aggDimCols + `,
	count(*), count(*) FILTER (WHERE error_type <> 'none'),
	sum(input_tokens), sum(output_tokens), sum(total_tokens),
	sum(cache_read_tokens), sum(cache_creation_tokens), sum(cost), COALESCE(sum(raw_cost), 0),
	sum(call_count),
	COALESCE(sum(ttft_ms), 0), count(ttft_ms), COALESCE(max(ttft_ms), 0), ` + aggHistExpr + `
FROM usage_logs WHERE created_at >= $1 AND created_at < $2 AND error_type IN ('none', 'abort')
GROUP BY 1, 2, 3`

// aggErrLogSQL err_logs → 纯错误桶补充（count 语义；WHERE error_type <> 'abort'
// 防双计——abort 行已由 aggUsageSQL 全字段计；spec §3.2）。**rc = count(\*)**
// ——err_logs 含豁免非错误行（error_type='none'），request_count 必须计入
// （Momus M1 勘误：勿按"rc=0"理解）；ec = FILTER (WHERE error_type <> 'none')。
// 瘦表无 tokens/cost/call_count/TTFT 列 → 恒 0 补位（11 测量列 + 全零直方图；
// 列序对齐扫描：raw 恒在 cost 后）。
var aggErrLogSQL = `SELECT ` + aggDimCols + `,
	count(*), count(*) FILTER (WHERE error_type <> 'none'),
	0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint,
	0::bigint, 0::bigint, 0::bigint, 0::bigint,
	ARRAY[0::bigint, 0, 0, 0, 0, 0, 0, 0, 0, 0]
FROM err_logs WHERE created_at >= $1 AND created_at < $2 AND error_type <> 'abort'
GROUP BY 1, 2, 3`

// aggRowKey cube 聚合行合并键（v2 三键 = 唯一索引列序；跨源撞 key 判定：
// usage_logs 行与 err_logs 行同维度可撞——合并累加；单查询内 GROUP BY 天然
// 无重复）。
type aggRowKey struct {
	bucketTime time.Time
	groupID    int64
	model      string
}

func aggKeyOf(b *domain.StatBucket) aggRowKey {
	return aggRowKey{bucketTime: b.BucketTime, groupID: b.GroupID, model: b.Model}
}

// LoadAggRange 离线聚合双结果集重建（spec 2026-08-23 §3）：重算范围 [from,to)
// 的小时桶全量重建——cube 两查询（usage_logs 单扫描放行行含 abort 全字段 +
// err_logs 纯错误桶补充，见 aggUsageSQL/aggErrLogSQL）+ 实体六查询（三实体
// 类型 × usage/errlog 两源，见 stat_entity_agg.go）。合并语义：跨源同 key 测量
// 列累加（cube：usage_logs 行 vs err_logs 行；entity 同理）。返回：cube 桶 +
// entity 桶（各自按键排序——确定性，重放同范围结果一致）+ 消费的明细行数
// （= cube 两查询 count(*) 合计；实体六查询扫的是同两表的同批行，不重复计数
// ——观测面"上轮行数"口径）。覆盖语义：SELECT 覆盖已消费行无害（DELETE 先清、
// INSERT 全量覆盖，幂等）。
func (r *StatRepo) LoadAggRange(ctx context.Context, from, to time.Time) ([]*domain.StatBucket, []*domain.EntityStatBucket, int64, error) {
	if r.pool == nil {
		return nil, nil, 0, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot aggregate offline range")
	}
	merged := make(map[aggRowKey]*domain.StatBucket)
	var detailRows int64
	for _, sql := range []string{aggUsageSQL, aggErrLogSQL} {
		rows, err := r.pool.Query(ctx, sql, from, to)
		if err != nil {
			return nil, nil, 0, err
		}
		for rows.Next() {
			// 每行独立扫描目标（pgx Scan 需逐行独立地址；列序 = aggDimCols +
			// 2 计数 + 11 测量（in/out/tot/cr/cc/cost/raw——raw 恒在 cost 后——
			// call + 3 TTFT）+ 直方图）。
			var (
				bt                                                                    time.Time
				g                                                                     int64
				m                                                                     string
				req, errN, in, out, tot, cr, cc, cost, raw, call, ttftS, ttftC, ttftM int64
				hist                                                                  []int64
			)
			if err := rows.Scan(&bt, &g, &m, &req, &errN, &in,
				&out, &tot, &cr, &cc, &cost, &raw, &call, &ttftS, &ttftC, &ttftM, &hist); err != nil {
				rows.Close()
				return nil, nil, 0, err
			}
			if len(hist) != len(ttftHistBounds) { // 防御：直方图档位漂移即刻显形
				rows.Close()
				return nil, nil, 0, fmt.Errorf("stat repo: agg hist len %d != %d (bucket %s)", len(hist), len(ttftHistBounds), bt)
			}
			b := &domain.StatBucket{
				BucketTime: bt, GroupID: g, Model: m,
				RequestCount: req, ErrorCount: errN,
				InputTokens: in, OutputTokens: out, TotalTokens: tot,
				CacheReadTokens: cr, CacheCreationTokens: cc, Cost: cost, RawCost: raw, CallCount: call,
				TTFTTotalMS: ttftS, TTFTCount: ttftC, TTFTMaxMS: ttftM, TTFTHist: hist,
			}
			detailRows += b.RequestCount
			key := aggKeyOf(b)
			if m2, ok := merged[key]; ok {
				mergeAggRow(m2, b)
			} else {
				merged[key] = b
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, 0, err
		}
		rows.Close()
	}
	cube := make([]*domain.StatBucket, 0, len(merged))
	for _, b := range merged {
		cube = append(cube, b)
	}
	sort.Slice(cube, func(i, j int) bool { return lessStatBucket(cube[i], cube[j]) })
	entity, err := r.loadEntityAggRange(ctx, from, to)
	if err != nil {
		return nil, nil, 0, err
	}
	return cube, entity, detailRows, nil
}

// mergeAggRow 跨源同 key 桶测量列累加（cube 双源合并；TTFT max 取大）。
func mergeAggRow(dst, src *domain.StatBucket) {
	dst.RequestCount += src.RequestCount
	dst.ErrorCount += src.ErrorCount
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.Cost += src.Cost
	dst.RawCost += src.RawCost
	dst.CallCount += src.CallCount
	dst.TTFTTotalMS += src.TTFTTotalMS
	dst.TTFTCount += src.TTFTCount
	if src.TTFTMaxMS > dst.TTFTMaxMS {
		dst.TTFTMaxMS = src.TTFTMaxMS
	}
	if dst.TTFTHist == nil {
		dst.TTFTHist = make([]int64, len(ttftHistBounds))
	}
	mergeHist(dst.TTFTHist, src.TTFTHist)
}

// lessStatBucket 桶确定性排序（LoadAggRange 输出与重放结果一致；排序键 =
// 唯一索引列序 bucket_time, group_id, model）。
func lessStatBucket(a, b *domain.StatBucket) bool {
	if !a.BucketTime.Equal(b.BucketTime) {
		return a.BucketTime.Before(b.BucketTime)
	}
	if a.GroupID != b.GroupID {
		return a.GroupID < b.GroupID
	}
	return a.Model < b.Model
}

// statsAggInsertCols 离线聚合 INSERT 列（与 usage_stats DDL v2 列序一致；不含
// id——DEFAULT nextval，ent bigserial 同款语义；raw_cost 紧随 cost）。
var statsAggInsertCols = []string{
	"bucket_time", "group_id", "model",
	"request_count", "error_count", "input_tokens", "output_tokens", "total_tokens",
	"cache_read_tokens", "cache_creation_tokens", "cost", "raw_cost", "call_count",
	"ttft_total_ms", "ttft_count", "ttft_max_ms", "ttft_hist", "updated_at",
}

// statsAggRowArgs 单桶 → INSERT 参数（列序 = statsAggInsertCols；TTFTHist 直接
// []int64 参数化——pgx v5 原生编码 bigint[]（spec ⚠️15 已核实，无 COPY 数组
// 路径）；nil/长度漂移防御回落全零直方图）。
func statsAggRowArgs(b *domain.StatBucket, now time.Time) []any {
	hist := b.TTFTHist
	if len(hist) != len(ttftHistBounds) {
		hist = ttftZeroHist
	}
	return []any{
		b.BucketTime, b.GroupID, b.Model,
		b.RequestCount, b.ErrorCount, b.InputTokens, b.OutputTokens, b.TotalTokens,
		b.CacheReadTokens, b.CacheCreationTokens, b.Cost, b.RawCost, b.CallCount,
		b.TTFTTotalMS, b.TTFTCount, b.TTFTMaxMS, hist, now,
	}
}

// AggregateRange 单事务覆盖落盘（spec 2026-08-14 §3.3 + spec 2026-08-23 §3.3
// 双表扩展）：DELETE cube [range] → INSERT cube → DELETE entity [range] →
// INSERT entity → watermark 推进 wmTo。**同一事务**——任一步失败整体回滚 →
// 游标不动 → 重算恢复不双计（双表原子：cube 失败则 entity 同回滚，反之亦然）；
// 重复执行同范围结果一致（覆盖语义，issue #8 教训：修正/补账通过重算 bucket
// 实现，非累加）。**wmTo = 读窗口 T（≠ 重算范围上界 delTo）**——watermark 推进
// 到 delTo 会永久跳过 [T, delTo) 的行（P1-A 要防的错误形态）；两范围分离由
// 调用方 worker 执行（见 usage/stats_agg.go）。Upsert（COPY+ON CONFLICT 累加）
// 语义不适用（覆盖语义，无双写者），已删除。
func (r *StatRepo) AggregateRange(ctx context.Context, delFrom, delTo, wmTo time.Time, cube []*domain.StatBucket, entity []*domain.EntityStatBucket) error {
	if r.pool == nil {
		return fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot aggregate offline range")
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // 任一步失败整体回滚（游标不动，重算恢复不双计）
	if _, err := tx.Exec(ctx, `DELETE FROM "usage_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2`, delFrom, delTo); err != nil {
		return err
	}
	now := time.Now()
	for start := 0; start < len(cube); start += statsAggBatchSize {
		end := min(start+statsAggBatchSize, len(cube))
		if err := insertStatBuckets(ctx, tx, cube[start:end], now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM "usage_entity_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2`, delFrom, delTo); err != nil {
		return err
	}
	for start := 0; start < len(entity); start += statsAggBatchSize {
		end := min(start+statsAggBatchSize, len(entity))
		if err := insertEntityStatBuckets(ctx, tx, entity[start:end], now); err != nil {
			return err
		}
	}
	// watermark 推进与 DELETE+INSERT 同一事务（单行表恒 id=1；UPSERT 形态——
	// 行缺失（初始化竞态窗口）也直接落位，不依赖 InitStatsAggWatermark 先行）。
	if _, err := tx.Exec(ctx, `INSERT INTO stats_agg_watermark (id, watermark) VALUES (1, $1) ON CONFLICT (id) DO UPDATE SET watermark = EXCLUDED.watermark`, wmTo); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// batchInsertExec 单条多值参数化 INSERT（占位符按行列动态构建；调用方保证单批
// ≤ statsAggBatchSize 行——18 列 × 500 ≈ 9k 参数，PG 参数上限 65535 安全）。
func batchInsertExec(ctx context.Context, tx pgx.Tx, table string, cols []string, argRows [][]any) error {
	var b strings.Builder
	b.WriteString(`INSERT INTO "`)
	b.WriteString(table)
	b.WriteString(`" (`)
	for i, col := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"`)
		b.WriteString(col)
		b.WriteString(`"`)
	}
	b.WriteString(`) VALUES `)
	args := make([]any, 0, len(argRows)*len(cols))
	for i, rowArgs := range argRows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`(`)
		for j := range rowArgs {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "$%d", i*len(cols)+j+1)
		}
		b.WriteString(`)`)
		args = append(args, rowArgs...)
	}
	_, err := tx.Exec(ctx, b.String(), args...)
	return err
}

// insertStatBuckets 参数化批量 INSERT（cube；列序 = statsAggInsertCols）。
func insertStatBuckets(ctx context.Context, tx pgx.Tx, rows []*domain.StatBucket, now time.Time) error {
	argRows := make([][]any, len(rows))
	for i, row := range rows {
		argRows[i] = statsAggRowArgs(row, now)
	}
	return batchInsertExec(ctx, tx, "usage_stats", statsAggInsertCols, argRows)
}

// AcquireStatsAggLock 抢占聚合 worker 会话级 advisory lock（pg_try_advisory_
// lock；**专用连接持有到 release**——池连接复用即丢锁，P3）。抢锁失败 →
// ok=false（本周期跳过，其他实例在聚合）。release 必须恰好调用一次（解锁 +
// 归还连接；解锁失败静默——连接归还后会话级锁随连接生命周期消失，无泄漏）。
func (r *StatRepo) AcquireStatsAggLock(ctx context.Context) (release func(), ok bool, err error) {
	if r.pool == nil {
		return nil, false, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot acquire stats agg lock")
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, statsAggLockKey).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, statsAggLockKey)
		conn.Release()
	}, true, nil
}

// LoadStatsAggWatermark 读聚合 watermark（zero time = 尚未初始化——全新库由
// worker 首轮初始化 now−滞后，防首跑扫全史）。
func (r *StatRepo) LoadStatsAggWatermark(ctx context.Context) (time.Time, error) {
	if r.pool == nil {
		return time.Time{}, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot read stats agg watermark")
	}
	var t time.Time
	err := r.pool.QueryRow(ctx, `SELECT watermark FROM stats_agg_watermark WHERE id = 1`).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

// InitStatsAggWatermark 全新库 watermark 初始化 = now − 滞后（防首跑扫全史 +
// DELETE 撞 retention 已 DROP 分区）。ON CONFLICT DO NOTHING——多实例并发初始化
// 撞单行键唯一约束容忍，败者重读既有值收敛。
func (r *StatRepo) InitStatsAggWatermark(ctx context.Context, t time.Time) error {
	if r.pool == nil {
		return fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot init stats agg watermark")
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO stats_agg_watermark (id, watermark) VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, t)
	return err
}

// —— /api/admin/overview 聚合面（spec 2026-08-14；SQL 侧 GROUP BY——F-P2-2 形态：
// 服务端分组返回日桶，不拉全行客户端聚合——720 万行/30 天客户端解码不可行） ——

// StatSummary 区间聚合单行（summary"今日"区间；SQL 侧单行 sum）。TTFT 指标：
// SQL 只 sum/count/max + 直方图 array_agg；avg/分位在查询侧 Go 除/插值
// （TTFTAvgMS/TTFTPercentileMS——pN 分母口径：仅含首 token 流式请求，TTFT
// 非 nil 行；abort 行含 TTFT 也计入其桶）。
type StatSummary struct {
	Requests        int64
	Errors          int64
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CacheReadTokens int64
	Cost            int64 // 毫分
	RawCost         int64 // 毫分（乘倍率前原始成本）
	CallCount       int64 // 按次调用（图片生成 = 张数、search = 1）
	TTFTTotalMS     int64
	TTFTCount       int64
	TTFTMaxMS       int64
	TTFTHist        []int64 // 合并后 10 档（len = 10；array_agg + Go 逐元素合并）
}

// StatDayAgg 单日聚合行（trend 日桶；date = UTC 日界）。TTFT 字段同
// StatSummary（日桶直方图合并 + Go 侧插值）。
type StatDayAgg struct {
	Date        time.Time
	Requests    int64
	Errors      int64
	Tokens      int64
	Cost        int64 // 毫分
	RawCost     int64 // 毫分（乘倍率前原始成本）
	CallCount   int64
	TTFTTotalMS int64
	TTFTCount   int64
	TTFTMaxMS   int64
	TTFTHist    []int64 // 合并后 10 档
}

// statSummarySQL 区间聚合单行（overview summary：今日区间 [from, to) 的测量列
// 全量 sum + 直方图 array_agg；groupID > 0 时追加组过滤）。sum(bigint) →
// numeric，显式 ::bigint 回落（pgx 扫描 int64 不受 numeric 精度语义干扰）；空
// 区间各列为 NULL → COALESCE 归零（summary 恒为全量结构，字段不因无数据缺席）；
// array_agg 空区间 NULL → COALESCE 空数组（pgx [][]int64 扫描）。
var statSummarySQL = `SELECT COALESCE(sum(request_count), 0)::bigint,
	COALESCE(sum(error_count), 0)::bigint,
	COALESCE(sum(input_tokens), 0)::bigint,
	COALESCE(sum(output_tokens), 0)::bigint,
	COALESCE(sum(total_tokens), 0)::bigint,
	COALESCE(sum(cache_read_tokens), 0)::bigint,
	COALESCE(sum(cost), 0)::bigint,
	COALESCE(sum(raw_cost), 0)::bigint,
	COALESCE(sum(call_count), 0)::bigint,
	COALESCE(sum(ttft_total_ms), 0)::bigint,
	COALESCE(sum(ttft_count), 0)::bigint,
	COALESCE(max(ttft_max_ms), 0)::bigint,
	COALESCE(ARRAY_AGG(ttft_hist) FILTER (WHERE ttft_hist IS NOT NULL), ARRAY[]::bigint[][])
FROM "usage_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2`

// statTrendSQL 日桶聚合（overview trend：近 N 天日桶；SQL 侧 GROUP BY
// date_trunc('day', bucket_time)——usage_stats 分区键，range 毫秒级）。直方图
// 每行 array_agg 带回，Go 侧逐元素合并。WHERE 后可追加组过滤（占位 $3），
// GROUP BY/ORDER BY 尾段单独常量（statTrendTailSQL）——过滤条件必须插在
// GROUP BY 之前。日界固定 UTC（评审 P2-1）：date_trunc('day', timestamptz) 按
// 会话 TimeZone 截断——非 UTC 会话下日桶边界与 summary 的 Go 侧 UTC 区间错位。
// 先 AT TIME ZONE 'UTC' 取 UTC 墙钟再截断、再转回 timestamptz（会话无关）。
var statTrendSQL = `SELECT date_trunc('day', "bucket_time" AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
	COALESCE(sum(request_count), 0)::bigint,
	COALESCE(sum(error_count), 0)::bigint,
	COALESCE(sum(total_tokens), 0)::bigint,
	COALESCE(sum(cost), 0)::bigint,
	COALESCE(sum(raw_cost), 0)::bigint,
	COALESCE(sum(call_count), 0)::bigint,
	COALESCE(sum(ttft_total_ms), 0)::bigint,
	COALESCE(sum(ttft_count), 0)::bigint,
	COALESCE(max(ttft_max_ms), 0)::bigint,
	COALESCE(ARRAY_AGG(ttft_hist) FILTER (WHERE ttft_hist IS NOT NULL), ARRAY[]::bigint[][])
FROM "usage_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2`

// statTrendTailSQL 日桶聚合尾段（GROUP BY 1 ORDER BY 1——组过滤拼接后追加）。
var statTrendTailSQL = ` GROUP BY 1 ORDER BY 1`

// SummarizeStats 区间聚合单行（SQL 侧 sum；overview summary——今日汇总同查
// 形态：单行不拉行）。groupID > 0 = 按组过滤（0 = 全局）。pool 未注入
// （非 NewWithPG 构造）→ 显式错误（不静默降级）。
func (r *StatRepo) SummarizeStats(ctx context.Context, from, to time.Time, groupID int64) (*StatSummary, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot aggregate overview summary")
	}
	sql := statSummarySQL
	args := []any{from, to}
	if groupID > 0 {
		sql += ` AND "group_id" = $3`
		args = append(args, groupID)
	}
	var rawHist [][]int64
	s := &StatSummary{}
	err := r.pool.QueryRow(ctx, sql, args...).Scan(
		&s.Requests, &s.Errors, &s.InputTokens, &s.OutputTokens,
		&s.TotalTokens, &s.CacheReadTokens, &s.Cost, &s.RawCost, &s.CallCount,
		&s.TTFTTotalMS, &s.TTFTCount, &s.TTFTMaxMS, &rawHist)
	if err != nil {
		return nil, err
	}
	s.TTFTHist = make([]int64, len(ttftHistBounds))
	for _, h := range rawHist { // array_agg 逐行直方图合并（加法交换序无关）
		mergeHist(s.TTFTHist, h)
	}
	return s, nil
}

// ScanStatsDays 日桶聚合（SQL 侧 GROUP BY date_trunc('day', bucket_time)；
// overview trend——服务端分组，不拉全行客户端聚合）。groupID > 0 = 按组
// 过滤（0 = 全局）。
func (r *StatRepo) ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64) ([]*StatDayAgg, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot aggregate overview trend")
	}
	sql := statTrendSQL
	args := []any{from, to}
	if groupID > 0 {
		sql += ` AND "group_id" = $3` // 组过滤插在 GROUP BY 之前（见 statTrendTailSQL）
		args = append(args, groupID)
	}
	sql += statTrendTailSQL
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*StatDayAgg{}
	for rows.Next() {
		d := &StatDayAgg{}
		var rawHist [][]int64
		if err := rows.Scan(&d.Date, &d.Requests, &d.Errors, &d.Tokens, &d.Cost, &d.RawCost,
			&d.CallCount, &d.TTFTTotalMS, &d.TTFTCount, &d.TTFTMaxMS, &rawHist); err != nil {
			return nil, err
		}
		d.TTFTHist = make([]int64, len(ttftHistBounds))
		for _, h := range rawHist {
			mergeHist(d.TTFTHist, h)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// OverviewResourceCounts 资源计数（overview resources：templates/groups/users
// 冷面 count；模板/分组排除软删——与列表端点口径一致）。单方法聚合三表计数
// （overview 一站式便捷面；ent client 查询，不走 pool）。
type OverviewResourceCounts struct {
	Templates int
	Groups    int
	Users     int
}

// CountOverviewResources 三表资源计数（overview resources 段）。
func (r *StatRepo) CountOverviewResources(ctx context.Context) (*OverviewResourceCounts, error) {
	tpls, err := r.client.Template.Query().Where(template.DeletedAtIsNil()).Count(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := r.client.Group.Query().Where(group.DeletedAtIsNil()).Count(ctx)
	if err != nil {
		return nil, err
	}
	users, err := r.client.User.Query().Count(ctx)
	if err != nil {
		return nil, err
	}
	return &OverviewResourceCounts{Templates: tpls, Groups: groups, Users: users}, nil
}
