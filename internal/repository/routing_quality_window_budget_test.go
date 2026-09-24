// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 预算门（spec §8 A10 的 CI 锁）：基线窗两轮读在「整段 24h 回看都有数据」的
// H=1000 夹具上必须 < 1500ms，且同夹具的整段回看单次读必须 > 1500ms（自校准）。
//
// WHY 必须 seed 整段回看：截断在 30 attempts 处停下（每行 10 attempts、每分钟
// 2 实例 ⇒ 前缀路径只碰最新 ~2 分钟）；若夹具只 seed 前缀，回落路径同样便宜，
// 预算就分不清「前缀优化在」与「前缀优化丢了」——门是空洞的。整段回看让丢掉
// 前缀的代价放大 ~48×（1435 分钟 vs ~30 分钟），门才有牙。
//
// WHY 自校准：1500ms 在不同 CI 机器上含义不同；断言整段回看单次读本身超预算，
// 证明本夹具在本机上确实区分得出两条路径。若该断言失败，是夹具退化（不再区分），
// 不是预算太严——此时必须修夹具，绝不放宽预算。
func TestRoutingQualityWindowBaselineBudgetPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := windowTestMinute
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	var rc domain.RouteClassIDVal
	rc[0] = 0xA1
	var qc domain.QualityClassIDVal
	qc[0] = 0xB2
	mkFP := func(marker byte, i int) domain.CandidateFingerprintVal {
		var fp domain.CandidateFingerprintVal
		fp[0] = byte(i >> 8)
		fp[1] = byte(i)
		fp[2] = marker
		return fp
	}

	const nhot = 1000
	keys := make([]repository.WindowHotKey, 0, nhot+2)
	for i := 0; i < nhot; i++ {
		keys = append(keys, repository.WindowHotKey{RouteClassID: rc, Fingerprint: mkFP(0xC3, i)})
	}
	// 两枚稀疏键只为练回落路径，不计入测量主体：partial 前缀内 20（<30）须回落
	// 拾起 m-2h 的旧行；deep 前缀内根本无行，删掉回落会静默丢它。
	sparsePartial := repository.WindowHotKey{RouteClassID: rc, Fingerprint: mkFP(0xD1, 1)}
	sparseDeep := repository.WindowHotKey{RouteClassID: rc, Fingerprint: mkFP(0xD2, 2)}
	keys = append(keys, sparsePartial, sparseDeep)

	// 整段回看 seeding：每候选 × 每分钟（m-1440m..m-6m = [M-24h, M-5m) 全覆盖）
	// × 2 实例，每行 10 attempts ⇒ 截断边界落在最新 ~2 分钟内，前缀外仍有
	// 1433 分钟的数据等着惩罚丢掉前缀的实现。Go 侧指纹 = (i 高字节, i 低字节,
	// 0xC3)，SQL 侧用 set_byte 逐字节重建，两边必须一致。
	seedStart := time.Now()
	const seedSQL = `INSERT INTO routing_quality_fact
		 (route_class_id, quality_class_id, candidate_fingerprint,
		  instance_src, bucket_minute, absolute_sequence, attempts, successes, updated_at)
		SELECT $1, $2,
		 set_byte(set_byte(set_byte(decode(repeat('00',32),'hex'), 0, (g.i/256)::int), 1, (g.i%256)::int), 2, 195),
		 inst.s,
		 $5::timestamptz - (gm.off || ' minutes')::interval,
		 1, 10, 7, now()
		FROM generate_series($3::int, $4::int) AS g(i)
		CROSS JOIN (VALUES ('src-a'), ('src-b')) AS inst(s)
		CROSS JOIN generate_series(6, 1440) AS gm(off)`
	for lo := 0; lo < nhot; lo += 250 {
		_, err := pool.Exec(ctx, seedSQL, rc[:], qc[:], lo, lo+249, m)
		require.NoError(t, err)
	}
	// 稀疏键：与批量指纹（marker 0xC3）不重叠，逐行种（行数极少）。
	seedSparse := func(fp domain.CandidateFingerprintVal, offs []int) {
		t.Helper()
		for _, off := range offs {
			for _, inst := range []string{"src-a", "src-b"} {
				_, err := pool.Exec(ctx, `INSERT INTO routing_quality_fact
					 (route_class_id, quality_class_id, candidate_fingerprint,
					  instance_src, bucket_minute, absolute_sequence, attempts, successes, updated_at)
					 VALUES ($1, $2, $3, $4, $5, 1, 10, 7, now())`,
					rc[:], qc[:], fp[:], inst, m.Add(-time.Duration(off)*time.Minute))
				require.NoError(t, err)
			}
		}
	}
	seedSparse(sparsePartial.Fingerprint, []int{6, 120})
	seedSparse(sparseDeep.Fingerprint, []int{180, 181, 182})
	pgExec(t, pool, `ANALYZE routing_quality_fact`)
	var nrows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_fact`).Scan(&nrows))
	t.Logf("seed: %d rows in %s (H=%d hot + 2 sparse, full 24h lookback, N=2)", nrows, time.Since(seedStart), nhot)

	// 预热一次（计划缓存与冷页），再各计时 3 轮，取 max（与 P99 锁同理，max
	// 才对「前缀优化整体丢失」这类让每次运行都变慢的回归敏感）。
	_, err := repos.Partitions.QueryBaselineTruncated(ctx, m, keys)
	require.NoError(t, err)
	const timedRounds = 3
	twoPhaseDurs := make([]time.Duration, 0, timedRounds)
	var twoPhaseRes []repository.WindowBaselineStat
	for i := 0; i < timedRounds; i++ {
		start := time.Now()
		twoPhaseRes, err = repos.Partitions.QueryBaselineTruncated(ctx, m, keys)
		require.NoError(t, err)
		twoPhaseDurs = append(twoPhaseDurs, time.Since(start))
	}
	fullDurs := make([]time.Duration, 0, timedRounds)
	var fullRes []repository.WindowBaselineStat
	for i := 0; i < timedRounds; i++ {
		start := time.Now()
		fullRes = queryFullLookbackBaseline(t, pool, m, keys)
		fullDurs = append(fullDurs, time.Since(start))
	}
	twoPhaseMax, fullMax := twoPhaseDurs[0], fullDurs[0]
	for i := 1; i < timedRounds; i++ {
		if twoPhaseDurs[i] > twoPhaseMax {
			twoPhaseMax = twoPhaseDurs[i]
		}
		if fullDurs[i] > fullMax {
			fullMax = fullDurs[i]
		}
	}
	t.Logf("baseline H=%d N=2 full-24h: two-phase max=%s %v, full-lookback one-shot max=%s %v (budget 1500ms)",
		nhot, twoPhaseMax, twoPhaseDurs, fullMax, fullDurs)

	const budget = 1500 * time.Millisecond
	require.Less(t, twoPhaseMax, budget, "两轮前缀路径必须在预算内；超预算说明前缀优化丢失或探针退化")
	require.Greater(t, fullMax, budget, "自校准失败：整段回看单次读在本夹具本机上都未超预算，夹具已失去区分度（分不清前缀路径与无前缀路径），该预算门已空洞——必须修夹具，不得放宽预算")

	// 大规模正确性：两轮读与整段回看单次读逐字段相等（与 TwoPhaseEquiv 同比较形状）。
	require.Len(t, twoPhaseRes, len(fullRes), "两轮读必须返回与整段回看完全相同的行集")
	for i := range twoPhaseRes {
		require.Equal(t, fullRes[i].RouteClassID, twoPhaseRes[i].RouteClassID)
		require.Equal(t, fullRes[i].Fingerprint, twoPhaseRes[i].Fingerprint)
		require.Equal(t, fullRes[i].Attempts, twoPhaseRes[i].Attempts, "attempts 必须与整段回看单次读相等")
		require.Equal(t, fullRes[i].Successes, twoPhaseRes[i].Successes, "successes 必须与整段回看单次读相等")
	}

	// 回落路径确实被练到：两枚稀疏键的值只有走回落才对（partial 前缀内仅 20，
	// deep 前缀内无行——删掉回落则前者变 20、后者直接消失）。
	byFP := map[string]repository.WindowBaselineStat{}
	for _, s := range twoPhaseRes {
		byFP[string(s.Fingerprint[:])] = s
	}
	require.Equal(t, int64(40), byFP[string(sparsePartial.Fingerprint[:])].Attempts, "partial 前缀内不足阈值，须回落拾起旧行")
	require.Equal(t, int64(28), byFP[string(sparsePartial.Fingerprint[:])].Successes)
	require.Equal(t, int64(40), byFP[string(sparseDeep.Fingerprint[:])].Attempts, "deep 前缀内无行，须回落读深层")
	require.Equal(t, int64(28), byFP[string(sparseDeep.Fingerprint[:])].Successes)
}
