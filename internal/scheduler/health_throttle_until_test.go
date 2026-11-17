package scheduler

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// 本文件钉住「冷却写入只延长」+「记录带绝对 until」的行为契约（设计 v3 §4/§6.2，
// 门禁 5/17/18/19/25/26）。全部用注入时钟 + miniredis：断言里的 PTTL/ExpiresAt
// 都是确定值，不依赖墙钟，也不用 sleep。
//
// 为什么不直接断言 Lua 脚本文本：文本会随注释/空格变化，而门禁要钉的是
// 「短冷却打不回长冷却」「31d 上界」「绝对 until 跨进程稳定」这些可观测事实。
// （「两脚本共享核心必须逐字相同」是另一类性质——它在行为上不可观察，见
// health_throttle_lua_test.go；那里不是把行为断言换成了文本断言。）

// fixedHealthNow 测试用的固定时刻（时区无关：比较一律走 UnixMilli）。
var fixedHealthNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// newFixedClockHealth 构造注入了固定时钟的 RuntimeHealth。
func newFixedClockHealth(t *testing.T, c *redis.Client, selfID string) (*RuntimeHealth, *time.Time) {
	t.Helper()
	now := fixedHealthNow
	h := NewRuntimeHealth(c, selfID, nil, nil)
	h.now = func() time.Time { return now }
	return h, &now
}

func TestThrottleUntilDoesNotShorten(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	key := healthKeyFor(11, "q1", 1)

	_, err := h.ThrottleUntil(context.Background(), key, StateRetryAfter, now.Add(24*time.Minute))
	require.NoError(t, err)
	// 后到的短冷却（典型：seed-5xx 的 10m）不得覆盖已有的 24m。
	_, err = h.ThrottleUntil(context.Background(), key, StateOPEN, now.Add(10*time.Minute))
	require.NoError(t, err)

	require.NoError(t, h.Sync(context.Background()))
	entry := h.View()[key]
	require.Equal(t, now.Add(24*time.Minute).UnixMilli(), entry.ExpiresAt, "ExpiresAt 必须停在更长的那次")
	require.Equal(t, (24 * time.Minute).Milliseconds(), entry.TTLms, "记录 TTL 必须停在更长的那次")

	pttl, err := c.PTTL(context.Background(), healthRecordPrefix+key.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 24*time.Minute, pttl, "Redis TTL 也只延长")
}

func TestThrottleUntilMultiInstanceMonotonic(t *testing.T) {
	_, c := newHealthTestRedis(t)
	hA, now := newFixedClockHealth(t, c, "self-a")
	hB, _ := newFixedClockHealth(t, c, "self-b")

	// 长 → 短（另一实例交错写）：不缩短。
	long := healthKeyFor(21, "q1", 1)
	_, err := hA.ThrottleUntil(context.Background(), long, StateRetryAfter, now.Add(24*time.Minute))
	require.NoError(t, err)
	_, err = hB.ThrottleUntil(context.Background(), long, StateOPEN, now.Add(10*time.Minute))
	require.NoError(t, err)
	pttl, err := c.PTTL(context.Background(), healthRecordPrefix+long.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 24*time.Minute, pttl)

	// 短 → 长：正常延长。
	grow := healthKeyFor(22, "q1", 1)
	_, err = hA.ThrottleUntil(context.Background(), grow, StateOPEN, now.Add(10*time.Minute))
	require.NoError(t, err)
	_, err = hB.ThrottleUntil(context.Background(), grow, StateRetryAfter, now.Add(24*time.Minute))
	require.NoError(t, err)
	pttl, err = c.PTTL(context.Background(), healthRecordPrefix+grow.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 24*time.Minute, pttl)
}

func TestThrottleUntilHardCap31d(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	key := healthKeyFor(31, "q1", 1)

	_, err := h.ThrottleUntil(context.Background(), key, StateRetryAfter, now.Add(90*24*time.Hour))
	require.NoError(t, err)

	// 行为上钉死「Go 常量 == Lua 字面量」：若两边不同步，这里的精确值就会红。
	pttl, err := c.PTTL(context.Background(), healthRecordPrefix+key.String()).Result()
	require.NoError(t, err)
	require.Equal(t, healthMaxTTL, pttl, "TTL 必须被 31d 上界截断")
	require.Equal(t, int64(2678400000), healthMaxTTL.Milliseconds(), "31d 的毫秒值变了就必须同步改 Lua 字面量")

	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, now.Add(healthMaxTTL).UnixMilli(), h.View()[key].ExpiresAt, "业务截止同样被截断")
}

func TestThrottleUntilRecordInvariants(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	key := healthKeyFor(41, "q1", 1)
	field := key.String()
	ctx := context.Background()

	// 预置该字段的墓碑（写路径必须把它清掉，否则 Sync 会把旧记录装回来）。
	require.NoError(t, c.HSet(ctx, healthTombstoneHash, field, "1").Err())
	require.NoError(t, c.Set(ctx, healthTombstonePrefix+field, "1", 0).Err())

	gen1, err := h.Throttle(ctx, key, StateOPEN, 5*time.Minute)
	require.NoError(t, err)
	gen2, err := h.Throttle(ctx, key, StateRetryAfter, 6*time.Minute)
	require.NoError(t, err)
	require.Greater(t, gen2, gen1, "每次写入都要推进 generation")

	rec := c.HGetAll(ctx, healthRecordPrefix+field).Val()
	require.Equal(t, "RETRY_AFTER", rec["state"])
	require.Equal(t, "1", rec["rev"])
	require.NotEmpty(t, rec["gen"])
	require.NotEmpty(t, rec["ttl"])
	require.NotEmpty(t, rec["until"], "记录必须带绝对 until")

	score, err := c.ZScore(ctx, healthActiveZSet, field).Result()
	require.NoError(t, err)
	require.Equal(t, float64(gen2), score, "活动 ZSET 的分值 = 最新 generation")

	tomb, err := c.HExists(ctx, healthTombstoneHash, field).Result()
	require.NoError(t, err)
	require.False(t, tomb, "墓碑哈希条目必须被清")
	exists, err := c.Exists(ctx, healthTombstonePrefix+field).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "墓碑前缀键必须被删")

	// 视图里的截止时刻来自 HASH 的 until，而不是「现在 + ttl」。
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, now.Add(6*time.Minute).UnixMilli(), h.View()[key].ExpiresAt)
}

func TestThrottleUntilAbsoluteUntil(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(51, "q1", 1)
	sevenDays := now.Add(7 * 24 * time.Hour)

	_, err := h.ThrottleUntil(ctx, key, StateOPEN, sevenDays)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))
	health, ok := h.AccountHealth(key.AccountID)
	require.True(t, ok)
	require.Equal(t, sevenDays.UnixMilli(), health.Until.UnixMilli(), "Until 必须等于业务截止")

	// 短写不覆盖长 until（业务截止也只延长）。
	_, err = h.ThrottleUntil(ctx, key, StateOPEN, now.Add(time.Hour))
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))
	health, ok = h.AccountHealth(key.AccountID)
	require.True(t, ok)
	require.Equal(t, sevenDays.UnixMilli(), health.Until.UnixMilli())

	// 旧记录（S2 之前写的：HASH 里没有 until）→ 回落 now+ttl，且不 panic。
	legacy := healthKeyFor(52, "q1", 1)
	legacyField := legacy.String()
	require.NoError(t, c.HSet(ctx, healthRecordPrefix+legacyField,
		"state", "OPEN", "gen", "1", "rev", "1", "ttl", "60000").Err())
	require.NoError(t, c.PExpire(ctx, healthRecordPrefix+legacyField, time.Minute).Err())
	require.NoError(t, c.ZAdd(ctx, healthActiveZSet, redis.Z{Score: 1, Member: legacyField}).Err())

	require.NoError(t, h.Sync(ctx))
	require.Equal(t, now.UnixMilli()+60000, h.View()[legacy].ExpiresAt, "缺 until 的旧记录回落 now+ttl")
}

// TestThrottleUntilKeepsTTLAlignedWithUntil 手工坏记录（until 7 天后但 TTL 只剩
// 1 小时）被再次写入时，TTL 必须补齐到 until 之后：否则记录会早于业务截止消失，
// 账号在窗口仍然打满时提前回到 READY。
func TestThrottleUntilKeepsTTLAlignedWithUntil(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(45, "q1", 1)
	field := key.String()

	until := now.Add(7 * 24 * time.Hour)
	require.NoError(t, c.HSet(ctx, healthRecordPrefix+field,
		"state", "OPEN", "gen", "1", "rev", "1",
		"ttl", strconv.FormatInt(time.Hour.Milliseconds(), 10),
		"until", strconv.FormatInt(until.UnixMilli(), 10)).Err())
	require.NoError(t, c.PExpire(ctx, healthRecordPrefix+field, time.Hour).Err())
	require.NoError(t, c.ZAdd(ctx, healthActiveZSet, redis.Z{Score: 1, Member: field}).Err())

	_, err := h.ThrottleUntil(ctx, key, StateOPEN, now.Add(2*time.Hour))
	require.NoError(t, err)
	pttl, err := c.PTTL(ctx, healthRecordPrefix+field).Result()
	require.NoError(t, err)
	require.Equal(t, 7*24*time.Hour, pttl, "TTL 必须补齐到 until 之后")
}

// TestThrottleUntilCapsLegacyOverlongUntil 历史坏记录（until/TTL 都是 60d）被再次
// 写入时，旧值参与 max 之后仍必须被 31d 上界夹住——否则「只延长」会把坏值永久
// 传播下去，冷却记录再也无法过期。
func TestThrottleUntilCapsLegacyOverlongUntil(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(46, "q1", 1)
	field := key.String()

	sixtyDays := now.Add(60 * 24 * time.Hour)
	require.NoError(t, c.HSet(ctx, healthRecordPrefix+field,
		"state", "OPEN", "gen", "1", "rev", "1",
		"ttl", strconv.FormatInt(sixtyDays.Sub(*now).Milliseconds(), 10),
		"until", strconv.FormatInt(sixtyDays.UnixMilli(), 10)).Err())
	require.NoError(t, c.PExpire(ctx, healthRecordPrefix+field, 60*24*time.Hour).Err())
	require.NoError(t, c.ZAdd(ctx, healthActiveZSet, redis.Z{Score: 1, Member: field}).Err())

	_, err := h.ThrottleUntil(ctx, key, StateOPEN, now.Add(5*time.Minute))
	require.NoError(t, err)

	pttl, err := c.PTTL(ctx, healthRecordPrefix+field).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, pttl, healthMaxTTL, "旧坏 TTL 不得把新记录带过 31d")

	storedUntil, err := strconv.ParseInt(c.HGet(ctx, healthRecordPrefix+field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.LessOrEqual(t, storedUntil, now.Add(healthMaxTTL).UnixMilli(), "旧坏 until 必须被夹回 31d")
}

// TestThrottleUntilPastDeadlineStaysReadable 过去的截止时刻必须被规范化成
// **正数** until：读路径把非正 until 当坏记录（冻结视图），所以这里既断言 1s 的
// 存活窗口，也断言随后一次 Sync 不会失败。
func TestThrottleUntilPastDeadlineStaysReadable(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(47, "q1", 1)
	field := key.String()

	_, err := h.ThrottleUntil(ctx, key, StateOPEN, now.Add(-time.Hour))
	require.NoError(t, err)

	pttl, err := c.PTTL(ctx, healthRecordPrefix+field).Result()
	require.NoError(t, err)
	require.Equal(t, time.Second, pttl, "过去截止 → 1s 存活窗口（与旧实现等价）")

	storedUntil, err := strconv.ParseInt(c.HGet(ctx, healthRecordPrefix+field, "until").Val(), 10, 64)
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Second).UnixMilli(), storedUntil, "until 必须规范化成正数")

	// 读路径不能因为这条记录冻结。
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, now.Add(time.Second).UnixMilli(), h.View()[key].ExpiresAt)
}

// TestMarkReadyClearsRecord READY 不是「带状态的记录」而是**没有记录**：readyLua
// 删 HASH、移出活动 ZSET、写墓碑。这条同时钉住读路径不会把 READY 之后再 Sync
// 的记录「复活」，也解释了为什么 READY 不需要绝对 until（根本没有记录可漂移）。
func TestMarkReadyClearsRecord(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, _ := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(48, "q1", 1)
	field := key.String()

	gen, err := h.Throttle(ctx, key, StateProbing, 30*time.Second)
	require.NoError(t, err)
	_, err = h.MarkReady(ctx, key, gen, 30*time.Second)
	require.NoError(t, err)

	exists, err := c.Exists(ctx, healthRecordPrefix+field).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "READY 之后记录必须消失（READY = 无记录）")
	tomb, err := c.HExists(ctx, healthTombstoneHash, field).Result()
	require.NoError(t, err)
	require.True(t, tomb, "READY 必须留墓碑，否则 Sync 会把旧记录装回来")

	require.NoError(t, h.Sync(ctx))
	_, ok := h.View()[key]
	require.False(t, ok, "READY 的键不得出现在健康视图里")
}

// TestThrottleUntilMultiQualityIsolation 账号级（quality="*"）与 route 质量类
// 记录是两条独立键：一方延长不得改写另一方。
func TestThrottleUntilMultiQualityIsolation(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()

	accountKey := healthKeyFor(49, "*", 1)
	routeKey := healthKeyFor(49, "q1", 1)

	_, err := h.ThrottleUntil(ctx, accountKey, StateRetryAfter, now.Add(24*time.Minute))
	require.NoError(t, err)
	_, err = h.ThrottleUntil(ctx, routeKey, StateOPEN, now.Add(10*time.Minute))
	require.NoError(t, err)

	accountTTL, err := c.PTTL(ctx, healthRecordPrefix+accountKey.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 24*time.Minute, accountTTL, "账号级记录保持 24m")

	routeTTL, err := c.PTTL(ctx, healthRecordPrefix+routeKey.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, routeTTL, "route 记录保持 10m（不被账号级带长）")
}

// TestLatchSinkPastResetAtStaysOneSecond use_reset 拿到过去的 ResetAt 时，行为必须
// 与旧实现等价：约 1 秒的冷却（不是 30s、也不是 0），且记录可被读路径正常消费。
func TestLatchSinkPastResetAtStaysOneSecond(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	sink := NewLatchSink(h, nil, nil, nil)
	ctx := context.Background()

	past := now.Add(-time.Hour)
	ev := rule.Event{AccountID: 63, ExpectedIdentityRevision: 1, CandidateFingerprint: testIdentity, ResetAt: &past}
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true}
	require.NoError(t, sink.Throttle(ev, th))

	field := HealthKey{AccountID: 63, Quality: "*", Identity: testIdentity, IdentityRevision: 1}.String()
	pttl, err := c.PTTL(ctx, healthRecordPrefix+field).Result()
	require.NoError(t, err)
	require.Equal(t, time.Second, pttl, "过去 ResetAt ⇒ 1s 存活窗口")
	require.NoError(t, h.Sync(ctx), "读路径不得因这条记录冻结")
}

// TestLatchSinkSeedsDoNotShorten：三类 seed 的时长来源不同（429 走 use_reset、
// 5xx 走 OPEN 10m、network 5s 级），S2 之后它们互相之间只延长、不缩短。
func TestLatchSinkSeedsDoNotShorten(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	sink := NewLatchSink(h, nil, nil, nil)
	ctx := context.Background()

	openFor := func(d time.Duration) domain.ThrottleAction {
		ms := d.Milliseconds()
		return domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: &ms}
	}
	retryWithReset := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true}

	// 24m 的 429 冷却在前，后到的 10m 5xx 不得缩短它。
	acc := int64(61)
	ev := rule.Event{AccountID: acc, ExpectedIdentityRevision: 1, CandidateFingerprint: testIdentity}
	reset := now.Add(24 * time.Minute)
	ev.ResetAt = &reset
	require.NoError(t, sink.Throttle(ev, retryWithReset))
	require.NoError(t, sink.Throttle(ev, openFor(10*time.Minute)))
	pttl, err := c.PTTL(ctx, healthRecordPrefix+HealthKey{AccountID: acc, Quality: "*", Identity: testIdentity, IdentityRevision: 1}.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 24*time.Minute, pttl, "429 的 24m 不得被 5xx 的 10m 打回")

	// 反向：5s 的 network 冷却在前，后到的 24m 正常延长。
	acc2 := int64(62)
	ev2 := rule.Event{AccountID: acc2, ExpectedIdentityRevision: 1, CandidateFingerprint: testIdentity}
	require.NoError(t, sink.Throttle(ev2, openFor(5*time.Second)))
	reset2 := now.Add(24 * time.Minute)
	ev2.ResetAt = &reset2
	require.NoError(t, sink.Throttle(ev2, retryWithReset))
	pttl, err = c.PTTL(ctx, healthRecordPrefix+HealthKey{AccountID: acc2, Quality: "*", Identity: testIdentity, IdentityRevision: 1}.String()).Result()
	require.NoError(t, err)
	require.Equal(t, 24*time.Minute, pttl)

	// 两个账号都应处于冷却态（不是 READY）。state 允许被后写覆盖（OPEN 与
	// RETRY_AFTER 在到期语义上同构），真正不能变的是 TTL/截止时刻。
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, StateOPEN, h.EffectiveState(acc, "*", testIdentity, 1), "后写的 5xx OPEN 覆盖了 state，但 TTL 仍是 24m")
	require.Equal(t, StateRetryAfter, h.EffectiveState(acc2, "*", testIdentity, 1))
}
