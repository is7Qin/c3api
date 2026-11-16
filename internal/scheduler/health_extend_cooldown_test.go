// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 本文件钉住 ExtendCooldown（429 补拉写回面）的闸门：只延长一条仍然存在、
// 状态相同、未过截止的冷却记录；拒绝（返回 0）不得产生任何写副作用。同时钉住
// latch 面 Throttle 的「可建」与 legacy 无 until 记录的「TTL 只延长」分支。
// 全部用注入时钟 + miniredis，断言 PTTL/until 都是确定值，不依赖墙钟。

// TestExtendCooldownRejectsMissingRecord 空库 extend ⇒ (0,nil)：不得新建记录、
// 不得推进 generation、不得写活动 ZSET。这是「迟到的补拉不能复活运维已清除的
// 冷却」的最底层断言。
func TestExtendCooldownRejectsMissingRecord(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(71, "q1", 1)

	require.NoError(t, c.Set(ctx, healthGenKey, "7", 0).Err())
	gen, err := h.ExtendCooldown(ctx, key, StateRetryAfter, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.Zero(t, gen, "记录不存在必须拒绝")

	exists, err := c.Exists(ctx, healthRecordPrefix+key.String()).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "拒绝不得新建记录")
	require.Equal(t, "7", c.Get(ctx, healthGenKey).Val(), "拒绝不得推进 generation")
	require.Zero(t, c.ZCard(ctx, healthActiveZSet).Val(), "拒绝不得写活动 ZSET")
}

// TestExtendCooldownRejectsOtherState 先 Throttle(OPEN,10m) 建记录，再
// ExtendCooldown(RETRY_AFTER,7d) ⇒ 0，记录仍是 OPEN/10m：迟到的 429 补拉不得
// 把 5xx 的 OPEN 判决改写成 RETRY_AFTER。
func TestExtendCooldownRejectsOtherState(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(72, "q1", 1)
	field := healthRecordPrefix + key.String()

	gen, err := h.Throttle(ctx, key, StateOPEN, 10*time.Minute)
	require.NoError(t, err)
	require.Greater(t, gen, int64(0))

	got, err := h.ExtendCooldown(ctx, key, StateRetryAfter, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.Zero(t, got, "异状态必须拒绝")

	require.Equal(t, "OPEN", c.HGet(ctx, field, "state").Val(), "state 不得被补拉改写")
	until, err := strconv.ParseInt(c.HGet(ctx, field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.Equal(t, now.Add(10*time.Minute).UnixMilli(), until, "until 不得被补拉改写")
	pttl, err := c.PTTL(ctx, field).Result()
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, pttl, "TTL 不得被补拉改写")
	require.Equal(t, strconv.FormatInt(gen, 10), c.Get(ctx, healthGenKey).Val(), "拒绝不得推进 generation")
}

// TestExtendCooldownRejectsExpiredDeadline 冷却已过截止（记录还在 Redis 里等
// TTL 清理）⇒ 补拉必须拒绝，不得把它复活成 7d。
func TestExtendCooldownRejectsExpiredDeadline(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(73, "q1", 1)
	field := healthRecordPrefix + key.String()

	gen, err := h.Throttle(ctx, key, StateRetryAfter, 2*time.Second)
	require.NoError(t, err)
	deadline := now.Add(2 * time.Second)
	*now = now.Add(3 * time.Second) // 注入时钟越过截止

	got, err := h.ExtendCooldown(ctx, key, StateRetryAfter, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.Zero(t, got, "已过截止的记录必须拒绝")

	until, err := strconv.ParseInt(c.HGet(ctx, field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.Equal(t, deadline.UnixMilli(), until, "已过截止的记录不得被复活/延长")
	require.Equal(t, "RETRY_AFTER", c.HGet(ctx, field, "state").Val())
	require.Equal(t, strconv.FormatInt(gen, 10), c.Get(ctx, healthGenKey).Val(), "拒绝不得推进 generation")
}

// TestExtendCooldownExtendsSameState 同状态、未过截止 ⇒ 正常延长：gen 递增、
// until 走到 7d、PTTL 同步到 168h。
func TestExtendCooldownExtendsSameState(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(74, "q1", 1)
	field := healthRecordPrefix + key.String()

	gen0, err := h.Throttle(ctx, key, StateRetryAfter, 30*time.Second)
	require.NoError(t, err)

	gen, err := h.ExtendCooldown(ctx, key, StateRetryAfter, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.Greater(t, gen, gen0, "延长必须推进 generation")
	require.Equal(t, strconv.FormatInt(gen, 10), c.Get(ctx, healthGenKey).Val())

	until, err := strconv.ParseInt(c.HGet(ctx, field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.Equal(t, now.Add(7*24*time.Hour).UnixMilli(), until)
	pttl, err := c.PTTL(ctx, field).Result()
	require.NoError(t, err)
	require.Equal(t, 7*24*time.Hour, pttl, "PTTL 必须与 until 对齐到 168h")
}

// TestExtendCooldownLegacyRecordNoUntil（N3）S2 之前的旧记录只有 ttl、没有
// until：`oldTTL > eff` 这条「TTL 只延长」分支是它唯一的受益者。补 10m 不得把
// 24m 的旧 TTL 打回 10m。
func TestExtendCooldownLegacyRecordNoUntil(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(75, "q1", 1)
	field := healthRecordPrefix + key.String()

	require.NoError(t, c.HSet(ctx, field,
		"state", "RETRY_AFTER", "gen", "1", "rev", "1",
		"ttl", strconv.FormatInt((24*time.Minute).Milliseconds(), 10)).Err())
	require.NoError(t, c.PExpire(ctx, field, 24*time.Minute).Err())
	require.NoError(t, c.ZAdd(ctx, healthActiveZSet, redis.Z{Score: 1, Member: key.String()}).Err())

	gen, err := h.ExtendCooldown(ctx, key, StateRetryAfter, now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Greater(t, gen, int64(0))

	pttl, err := c.PTTL(ctx, field).Result()
	require.NoError(t, err)
	require.GreaterOrEqual(t, pttl, 24*time.Minute, "旧记录的长 TTL 必须被保留（只延长）")
}

// TestExtendCooldownToleratesMalformedUntil（N7）字段存在但非数字时
// `tonumber` 得 nil，旧实现会在 `nil > number` 处抛 Lua 错、整键在记录 TTL 内
// 永久写不进。坏值必须按 0 处理，让本次写入正常落盘。
func TestExtendCooldownToleratesMalformedUntil(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(76, "q1", 1)
	field := healthRecordPrefix + key.String()

	require.NoError(t, c.HSet(ctx, field,
		"state", "RETRY_AFTER", "gen", "1", "rev", "1",
		"ttl", "60000", "until", "garbage").Err())
	require.NoError(t, c.PExpire(ctx, field, time.Minute).Err())
	require.NoError(t, c.ZAdd(ctx, healthActiveZSet, redis.Z{Score: 1, Member: key.String()}).Err())

	gen, err := h.ExtendCooldown(ctx, key, StateRetryAfter, now.Add(10*time.Minute))
	require.NoError(t, err, "坏 until 不得让整键写不进")
	require.Greater(t, gen, int64(0))

	until, err := strconv.ParseInt(c.HGet(ctx, field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.Equal(t, now.Add(10*time.Minute).UnixMilli(), until, "坏 until 按 0 处理，本次截止正常落盘")
	pttl, err := c.PTTL(ctx, field).Result()
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, pttl)
}

// TestThrottleCreatesRecord（N8）latch 面 Throttle 在空库上必须能建记录：单独
// 钉住「可建」语义，不让它只被 sink 的用例顺带覆盖。
func TestThrottleCreatesRecord(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(77, "q1", 1)
	field := healthRecordPrefix + key.String()

	gen, err := h.Throttle(ctx, key, StateRetryAfter, 30*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen, int64(0), "latch 面必须能在空库上创建记录")

	require.Equal(t, "RETRY_AFTER", c.HGet(ctx, field, "state").Val())
	until, err := strconv.ParseInt(c.HGet(ctx, field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.Equal(t, now.Add(30*time.Second).UnixMilli(), until)
	pttl, err := c.PTTL(ctx, field).Result()
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, pttl)
	require.Equal(t, strconv.FormatInt(gen, 10), c.Get(ctx, healthGenKey).Val())
}
