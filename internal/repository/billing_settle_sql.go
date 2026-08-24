// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_settle_sql.go 结算语句事实源（F2-opt v2 三车道拓扑，spec-f2opt-settlement
// §〇-b/§一/D7；wave3 D-C 桶级并行）：两车道自包含 CTE + 毒行梯子队头探针。
// 编排/事务/守卫逻辑见 billing_settle.go；本文件只承载 SQL 文本（纯数据表）。
//
// 桶谓词（wave3 D-C）：batch/temp_pool/spill 各 CTE 追加
// COALESCE(user_id, 0) % $2 = $3（args = [limit, K, bucket]）——桶间 uid 集合
// 不相交 → users/temp_balances 行锁集不相交（无死锁构造性保证）。**Momus 必改
// 落实：裸 user_id 对 NULL 匿名行取模为 NULL → 永不命中任何桶 = 游标永久搁浅，
// 必须 COALESCE 与 batch uid 定义同构。**

// settleBalanceSQL Balance 车道结算语句（§一原设计）：终 SELECT 首行为聚合哨兵
// （uid=-1 恒一行，ORDER BY 置首），其余为 debited/forced 的 (uid,balance_after)
// 定向余额对（oracle 必改 #3——Balances.Set 预检新鲜度）。
const settleBalanceSQL = `WITH batch AS (
	SELECT id, COALESCE(user_id, 0) AS uid, cost
	FROM usage_logs
	WHERE NOT billed AND error_type IN ('none', 'abort') AND cost > 0
		AND COALESCE(user_id, 0) % $2 = $3
		AND COALESCE(user_id, 0) NOT IN (
			SELECT user_id FROM temp_balances
			WHERE amount > 0 AND (expires_at IS NULL OR expires_at > now()))
	ORDER BY id LIMIT $1),
totals AS (SELECT uid, SUM(cost)::numeric AS delta FROM batch GROUP BY uid),
debited AS (
	UPDATE users u SET balance = u.balance - t.delta
	FROM totals t WHERE u.id = t.uid AND u.balance >= t.delta
	RETURNING u.id AS uid, u.balance AS balance_after),
forced AS (
	UPDATE users u SET balance = u.balance - t.delta
	FROM totals t WHERE u.id = t.uid AND u.id NOT IN (SELECT uid FROM debited)
	RETURNING u.id AS uid, u.balance AS balance_after),
od_map AS (
	SELECT uid, TRUE AS od FROM forced
	UNION ALL
	SELECT uid, FALSE AS od FROM debited),
marked AS (
	UPDATE usage_logs l SET billed = TRUE,
		overdraft = COALESCE(q.od, FALSE)
	FROM (
		SELECT b.id, b.uid, m.od
		FROM batch b LEFT JOIN od_map m ON m.uid = b.uid) q
	WHERE l.id = q.id AND NOT l.billed
	RETURNING l.id),
ghosts AS (
	SELECT b.uid FROM batch b
	WHERE NOT EXISTS (SELECT 1 FROM debited d WHERE d.uid = b.uid)
		AND NOT EXISTS (SELECT 1 FROM forced f WHERE f.uid = b.uid))
SELECT -1::bigint, -1::bigint,
	(SELECT COUNT(*) FROM batch)::bigint,
	(SELECT COUNT(*) FROM debited)::bigint,
	(SELECT COUNT(*) FROM forced)::bigint,
	(SELECT COUNT(*) FROM marked)::bigint,
	(SELECT COUNT(*) FROM ghosts)::bigint
UNION ALL
SELECT uid, balance_after, 0, 0, 0, 0, 0 FROM debited
UNION ALL
SELECT uid, balance_after, 0, 0, 0, 0, 0 FROM forced
ORDER BY 1`

// settleFefoSQL Temp 车道集合化 FEFO 结算语句（D7）：FEFO 序（expires ASC NULLS
// LAST）、行级条件扣（amount>=take）、部分覆盖三语义经 rn/cum 窗口函数集合化
// 保持；cum 显式 ROWS 帧——默认 RANGE 帧在 expires_at 并列时会给并列行相同
// cum，边界部分扣公式失真（少扣差额误入余额），ROWS 帧逐行累加消除并列歧义。
// spill>0 用户进余额条件扣→透支补刀→幽灵隔离，链形与 Balance 车道同构；
// Σdrawn 一致性结构性成立（spill 由实际 drawn 推导，条件扣丢行自动并入 spill）。
const settleFefoSQL = `WITH batch AS (
	SELECT id, COALESCE(user_id, 0) AS uid, cost
	FROM usage_logs
	WHERE NOT billed AND error_type IN ('none', 'abort') AND cost > 0
		AND COALESCE(user_id, 0) % $2 = $3
		AND COALESCE(user_id, 0) IN (
			SELECT user_id FROM temp_balances
			WHERE amount > 0 AND (expires_at IS NULL OR expires_at > now()))
	ORDER BY id LIMIT $1),
totals AS (SELECT uid, SUM(cost)::numeric AS delta FROM batch GROUP BY uid),
temp_pool AS (
	SELECT t.id AS tid, t.user_id AS uid, t.amount,
		ROW_NUMBER() OVER (PARTITION BY t.user_id ORDER BY t.expires_at NULLS LAST) AS rn,
		SUM(t.amount) OVER (PARTITION BY t.user_id ORDER BY t.expires_at NULLS LAST
			ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS cum
	FROM temp_balances t
	WHERE t.user_id IN (SELECT uid FROM totals) AND t.amount > 0
		AND (t.expires_at IS NULL OR t.expires_at > now())),
takes AS (
	SELECT p.tid, p.uid,
		CASE WHEN p.cum <= t.delta THEN p.amount
			ELSE t.delta - (p.cum - p.amount) END AS take
	FROM temp_pool p JOIN totals t ON t.uid = p.uid
	WHERE p.cum - p.amount < t.delta),
temp_drawn AS (
	UPDATE temp_balances tt SET amount = tt.amount - x.take
	FROM takes x WHERE tt.id = x.tid AND tt.amount >= x.take
	RETURNING x.uid AS uid, x.take),
spill AS (
	SELECT t.uid, t.delta - COALESCE(SUM(d.take), 0) AS spill
	FROM totals t LEFT JOIN temp_drawn d ON d.uid = t.uid
	GROUP BY t.uid, t.delta
	HAVING t.delta - COALESCE(SUM(d.take), 0) > 0),
debited AS (
	UPDATE users u SET balance = u.balance - s.spill
	FROM spill s WHERE u.id = s.uid AND u.balance >= s.spill
	RETURNING u.id AS uid, u.balance AS balance_after),
forced AS (
	UPDATE users u SET balance = u.balance - s.spill
	FROM spill s WHERE u.id = s.uid AND u.id NOT IN (SELECT uid FROM debited)
	RETURNING u.id AS uid, u.balance AS balance_after),
od_map AS (
	SELECT uid, TRUE AS od FROM forced
	UNION ALL
	SELECT uid, FALSE AS od FROM debited),
marked AS (
	UPDATE usage_logs l SET billed = TRUE,
		overdraft = COALESCE(q.od, FALSE)
	FROM (
		SELECT b.id, b.uid, m.od
		FROM batch b LEFT JOIN od_map m ON m.uid = b.uid) q
	WHERE l.id = q.id AND NOT l.billed
	RETURNING l.id),
ghosts AS (
	SELECT b.uid FROM batch b JOIN spill s ON s.uid = b.uid
	WHERE NOT EXISTS (SELECT 1 FROM debited d WHERE d.uid = b.uid)
		AND NOT EXISTS (SELECT 1 FROM forced f WHERE f.uid = b.uid))
SELECT -1::bigint, -1::bigint,
	(SELECT COUNT(*) FROM batch)::bigint,
	(SELECT COUNT(*) FROM debited)::bigint,
	(SELECT COUNT(*) FROM forced)::bigint,
	(SELECT COUNT(*) FROM marked)::bigint,
	(SELECT COUNT(*) FROM ghosts)::bigint
UNION ALL
SELECT uid, balance_after, 0, 0, 0, 0, 0 FROM debited
UNION ALL
SELECT uid, balance_after, 0, 0, 0, 0, 0 FROM forced
ORDER BY 1`

// probeBalanceHeadSQL / probeFefoHeadSQL 毒行梯子只读探针（对应车道 batch 谓词
// 同构含桶谓词——args = [K, bucket]；wave3 D-C Momus 必改：探针缺桶谓词会误中
// 他桶队头健康行 = 错写销），ORDER BY id LIMIT 1：K 次连续失败后定位该车道该桶
// 队头行供 MarkBilledBulk 写销隔离——确定性毒行纯 LIMIT 重试永不收敛（oracle
// 必改 #2），隔离必须命中失败车道自己的队头而非全局首行（否则误写销无辜行且
// 毒行仍在）。
const probeBalanceHeadSQL = `SELECT id FROM usage_logs
WHERE NOT billed AND error_type IN ('none', 'abort') AND cost > 0
	AND COALESCE(user_id, 0) % $1 = $2
	AND COALESCE(user_id, 0) NOT IN (
		SELECT user_id FROM temp_balances
		WHERE amount > 0 AND (expires_at IS NULL OR expires_at > now()))
ORDER BY id LIMIT 1`

const probeFefoHeadSQL = `SELECT id FROM usage_logs
WHERE NOT billed AND error_type IN ('none', 'abort') AND cost > 0
	AND COALESCE(user_id, 0) % $1 = $2
	AND COALESCE(user_id, 0) IN (
		SELECT user_id FROM temp_balances
		WHERE amount > 0 AND (expires_at IS NULL OR expires_at > now()))
ORDER BY id LIMIT 1`
