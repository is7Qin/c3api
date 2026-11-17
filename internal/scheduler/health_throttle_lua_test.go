// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 本文件钉住「拆成 throttleLua / extendCooldownLua 之后两侧仍然同源」。
//
// 拆脚本把共享核心抄成两份。只要没有红点，某一侧的保护（31d 上界、坏 until
// 兜底、历史坏 oldUntil 夹取、过去 until 规范化、只延长、oldTTL>eff、六步写入
// 顺序、拒绝/成功返回值）就会静默死掉——spec §1.1 的「只有单侧红点」表就是在
// 量这个。三件武器：
//
//  1. TestCooldownWriteMirror —— 同一张表跑两遍（latch / extend 各一列），覆盖
//     spec §1.1 的 8 条共享不变量；确实只属于一列的行用 only + why 显式标注，
//     并由 require 机械校验「标注必须写理由」。
//  2. TestCooldownScriptsShareVerbatimCore —— 机械证明两份共享核心逐字相同，
//     且 extend 唯一的代码差异就是补拉三闸。
//  3. TestCooldownScriptsWriteStepOrder —— 六步写入的相对顺序。
//
// 第 2/3 条是**结构**钉：它钉的不是行为（行为归第 1 条与既有用例），而是「两份
// 手抄必须一致」「顺序不许重排」这类行为上不可观察、但分叉即安全不变量被破的
// 事实。这不是把行为断言换成文本断言——理由见 health_throttle_until_test.go 文件头。

const (
	cooldownColLatch  = "latch"
	cooldownColExtend = "extend"
)

// cooldownColumn 是镜像表的一列：同一行的 seed/until/check 完全共用，只有被调用
// 的方法不同。
type cooldownColumn struct {
	name string
	// write 只差方法名：latch 面 ThrottleUntil（可建可改），补拉面 ExtendCooldown
	// （存在 + 同 state + 未过截止）。
	write func(h *RuntimeHealth, key HealthKey, until time.Time) (int64, error)
	// canCreate 该列能否在空库上建记录：补拉面不允许（见三闸）。
	canCreate bool
}

func cooldownColumns() []cooldownColumn {
	ctx := context.Background()
	return []cooldownColumn{
		{
			name:      cooldownColLatch,
			canCreate: true,
			write: func(h *RuntimeHealth, key HealthKey, until time.Time) (int64, error) {
				return h.ThrottleUntil(ctx, key, StateRetryAfter, until)
			},
		},
		{
			name:      cooldownColExtend,
			canCreate: false,
			write: func(h *RuntimeHealth, key HealthKey, until time.Time) (int64, error) {
				return h.ExtendCooldown(ctx, key, StateRetryAfter, until)
			},
		},
	}
}

// cooldownSeed 是写入前的「旧记录卫生」。两列共用同一份输入，否则同一行的两列就
// 不是在测同一件事。extend 列的前置记录必须是 state=RETRY_AFTER 且未过截止，
// 否则三闸先拒绝、共享核心根本跑不到（这一点由 harness 机械校验）。
type cooldownSeed struct {
	state    string
	rev      string
	gen      string
	until    *time.Duration // nil = 旧记录没有 until 字段（S2 之前的 legacy）
	untilRaw string         // 非空则逐字写进 until（坏值用例）
	ttlField time.Duration  // HASH 里的 ttl 字段
	pttl     time.Duration  // 记录自身的过期时间
	active   bool           // 写活动 ZSET（score = gen 字段）
	tombHash bool           // 预置墓碑哈希条目（六步写入的 HDEL 断言用）
	tombKey  bool           // 预置墓碑前缀键（六步写入的 DEL 断言用）
	genKey   string         // 非空则预置全局 generation 键
}

func (s *cooldownSeed) apply(t *testing.T, ctx context.Context, c *redis.Client, field string, now time.Time) {
	t.Helper()
	if s == nil {
		return
	}
	kv := make([]string, 0, 10)
	add := func(k, v string) { kv = append(kv, k, v) }
	if s.state != "" {
		add("state", s.state)
	}
	if s.gen != "" {
		add("gen", s.gen)
	}
	if s.rev != "" {
		add("rev", s.rev)
	}
	if s.ttlField != 0 {
		add("ttl", strconv.FormatInt(s.ttlField.Milliseconds(), 10))
	}
	switch {
	case s.untilRaw != "":
		add("until", s.untilRaw)
	case s.until != nil:
		add("until", strconv.FormatInt(now.Add(*s.until).UnixMilli(), 10))
	}
	require.NoError(t, c.HSet(ctx, healthRecordPrefix+field, kv).Err())
	if s.pttl != 0 {
		require.NoError(t, c.PExpire(ctx, healthRecordPrefix+field, s.pttl).Err())
	}
	if s.active {
		score, err := strconv.ParseInt(s.gen, 10, 64)
		require.NoError(t, err)
		require.NoError(t, c.ZAdd(ctx, healthActiveZSet, redis.Z{Score: float64(score), Member: field}).Err())
	}
	if s.tombHash {
		require.NoError(t, c.HSet(ctx, healthTombstoneHash, field, "1").Err())
	}
	if s.tombKey {
		require.NoError(t, c.Set(ctx, healthTombstonePrefix+field, "1", 0).Err())
	}
	if s.genKey != "" {
		require.NoError(t, c.Set(ctx, healthGenKey, s.genKey, 0).Err())
	}
}

// retryAfterSeed 是一条正常的补拉面前置记录：state 与两列写入的 state 相同
// （异状态会被 extend 闸门拒绝），until 与 TTL 都是确定值。
func retryAfterSeed(until, ttl time.Duration) *cooldownSeed {
	u := until
	return &cooldownSeed{
		state:    "RETRY_AFTER",
		rev:      "1",
		gen:      "1",
		until:    &u,
		ttlField: ttl,
		pttl:     ttl,
		active:   true,
	}
}

// malformedUntilSeed 是「字段存在但不是数字」的旧记录（N7）：Lua 的 tonumber 得
// nil，必须靠 `or 0` 兜底，否则整键在记录 TTL 内永久写不进。
func malformedUntilSeed() *cooldownSeed {
	return &cooldownSeed{
		state:    "RETRY_AFTER",
		rev:      "1",
		gen:      "1",
		untilRaw: "garbage",
		ttlField: time.Minute,
		pttl:     time.Minute,
		active:   true,
	}
}

// cooldownFixture 是一行×一列的执行结果 + 断言所需的全部上下文。
type cooldownFixture struct {
	t     *testing.T
	ctx   context.Context
	c     *redis.Client
	h     *RuntimeHealth
	col   cooldownColumn
	key   HealthKey
	field string
	now   time.Time
	seed  *cooldownSeed
	gen   int64  // 本次写入返回的 generation（拒绝时 0）
	gen0  string // 写入前的全局 generation 键（不存在 = ""）
	err   error
}

func (fx cooldownFixture) record() map[string]string {
	return fx.c.HGetAll(fx.ctx, healthRecordPrefix+fx.field).Val()
}

func (fx cooldownFixture) pttl() time.Duration {
	fx.t.Helper()
	pttl, err := fx.c.PTTL(fx.ctx, healthRecordPrefix+fx.field).Result()
	require.NoError(fx.t, err)
	return pttl
}

func (fx cooldownFixture) untilMs() int64 {
	fx.t.Helper()
	raw, err := strconv.ParseInt(fx.record()["until"], 10, 64)
	require.NoError(fx.t, err, "记录必须带可解析的绝对 until")
	return raw
}

func (fx cooldownFixture) genKeyValue() string {
	return fx.c.Get(fx.ctx, healthGenKey).Val()
}

// mustSucceed 断言写入成功：无错、返回 gen>0、且全局 generation 就是它。
func (fx cooldownFixture) mustSucceed() {
	fx.t.Helper()
	require.NoError(fx.t, fx.err)
	require.Greater(fx.t, fx.gen, int64(0), "%s 面必须写入成功", fx.col.name)
	require.Equal(fx.t, strconv.FormatInt(fx.gen, 10), fx.genKeyValue(), "INCR 之后的全局 generation 必须等于返回值")
}

// wantUntil 断言落盘的绝对 until（相对 now）。调用方给一句话说明这条不变量。
func (fx cooldownFixture) wantUntil(rel time.Duration, msg string) {
	fx.t.Helper()
	require.Equal(fx.t, fx.now.Add(rel).UnixMilli(), fx.untilMs(), msg)
}

// wantPTTL 断言记录寿命（= eff）。
func (fx cooldownFixture) wantPTTL(d time.Duration, msg string) {
	fx.t.Helper()
	require.Equal(fx.t, d, fx.pttl(), msg)
}

// cooldownWriteCase 是镜像表的一行：seed/until 是两列共用的输入，check 是两列
// 共用的断言；随列变化的只有「调哪个方法」。only 非空 = 该行只对一列成立，必须
// 同时写 why，否则就是「一侧无红点」被悄悄放过。
type cooldownWriteCase struct {
	name  string
	seed  func(col string) *cooldownSeed
	until time.Duration
	only  string
	why   string
	check func(fx cooldownFixture)
}

// TestCooldownWriteMirror 是 spec §1.1 的镜像表：同一张表对 latch / extend 各跑
// 一列。任何一条共享不变量只要在某一侧没红点，拆脚本就等于删掉了那侧的保护。
func TestCooldownWriteMirror(t *testing.T) {
	cases := []cooldownWriteCase{
		{
			// spec 行 1：31d 硬上界（Lua 字面量 2678400000）。拆开后原本只有 latch
			// 侧有红点（TestThrottleUntilHardCap31d），extend 侧一份抄错没人管。
			name: "31d硬上界",
			seed: func(col string) *cooldownSeed {
				if col == cooldownColLatch {
					return nil // latch 面空库即可建
				}
				return retryAfterSeed(10*time.Minute, 10*time.Minute)
			},
			until: 90 * 24 * time.Hour,
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				fx.wantPTTL(healthMaxTTL, "PTTL 必须被 31d 上界截断")
				fx.wantUntil(healthMaxTTL, "业务截止同样被截断")
			},
		},
		{
			// spec 行 2：坏 until 的 tonumber(...) or 0。原本只有 extend 侧有红点。
			name:  "坏until兜底",
			seed:  func(string) *cooldownSeed { return malformedUntilSeed() },
			until: 10 * time.Minute,
			check: func(fx cooldownFixture) {
				require.NoError(fx.t, fx.err, "坏 until 不得让整键写不进（Lua 报错）")
				fx.mustSucceed()
				fx.wantUntil(10*time.Minute, "坏 until 按 0 处理，本次截止正常落盘")
				fx.wantPTTL(10*time.Minute, "eff 取自本次截止")
			},
		},
		{
			// spec 行 3：历史坏 oldUntil 夹取（旧 until 是 60d）。原本只有 latch 有红点。
			name:  "历史坏oldUntil夹取",
			seed:  func(string) *cooldownSeed { return retryAfterSeed(60*24*time.Hour, 60*24*time.Hour) },
			until: 5 * time.Minute,
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				fx.wantPTTL(healthMaxTTL, "旧坏 TTL 不得把新记录带过 31d")
				fx.wantUntil(healthMaxTTL, "旧坏 until 必须被夹回 31d")
			},
		},
		{
			// spec 行 4：过去 until ⇒ now+1s。补拉面要看见这条规范化，旧 until 必须
			// 是坏值（oldUntil=0），否则三闸会以「已过截止」拒绝、或 oldUntil>untilMs
			// 保留旧值把它盖住；记录自身给 500ms 寿命，好让 eff 仍由 1s 决定。
			name: "过去until规范化",
			seed: func(col string) *cooldownSeed {
				if col == cooldownColLatch {
					return nil
				}
				s := malformedUntilSeed()
				s.ttlField = 500 * time.Millisecond
				s.pttl = 500 * time.Millisecond
				return s
			},
			until: -time.Hour,
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				fx.wantUntil(time.Second, "过去截止必须规范成正数 now+1s")
				fx.wantPTTL(time.Second, "存活窗口 1s（与旧实现等价）")
			},
		},
		{
			// spec 行 5：oldUntil > untilMs ⇒ 保留旧值（"只延长"的全部语义）。
			name:  "只延长",
			seed:  func(string) *cooldownSeed { return retryAfterSeed(7*24*time.Hour, 7*24*time.Hour) },
			until: 10 * time.Minute,
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				fx.wantUntil(7*24*time.Hour, "后到的短冷却不得覆盖已有长冷却")
				fx.wantPTTL(7*24*time.Hour, "TTL 同样不缩短")
			},
		},
		{
			// spec 行 6：eff = until-now 与 oldTTL > eff。legacy 记录（没有 until 字段）
			// 是 oldTTL 分支唯一的受益者：本次 10m 短于旧 24m TTL，记录寿命必须留 24m，
			// 而落盘的 until 仍是本次的 now+10m（两条独立的 max）。
			name: "oldTTL大于eff",
			seed: func(string) *cooldownSeed {
				return &cooldownSeed{
					state:    "RETRY_AFTER",
					rev:      "1",
					gen:      "1",
					ttlField: 24 * time.Minute,
					pttl:     24 * time.Minute,
					active:   true,
				}
			},
			until: 10 * time.Minute,
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				fx.wantPTTL(24*time.Minute, "旧记录的长 TTL 必须被保留（TTL 只延长）")
				fx.wantUntil(10*time.Minute, "until 与 TTL 各自取 max：本次截止仍然落盘")
			},
		},
		{
			// spec 行 7：INCR→HSET(state,gen,rev,ttl,until)→PEXPIRE→ZADD→HDEL→DEL。
			// 六步在一条 Lua 里，顺序错无法从外部「看」到，但停在哪一步能：把旧记录的
			// rev 写成 9、两条墓碑都预置好、全局 generation 预置成 40，然后逐项验证
			// 结果。rev 栅栏交给 readyLua：它要求记录 gen/rev 都等于本次值。
			name: "六步写入顺序",
			seed: func(string) *cooldownSeed {
				s := retryAfterSeed(10*time.Minute, 10*time.Minute)
				s.rev = "9"
				s.genKey = "40"
				s.tombHash = true
				s.tombKey = true
				return s
			},
			until: 20 * time.Minute,
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				rec := fx.record()
				require.Equal(fx.t, "RETRY_AFTER", rec["state"], "HSET 必须写入本次 state")
				require.Equal(fx.t, strconv.FormatInt(fx.gen, 10), rec["gen"], "HSET 必须写入本次 generation")
				require.Equal(fx.t, "1", rec["rev"], "HSET 必须写入键的 revision（否则 readyLua 永远 stale）")
				require.Equal(fx.t, strconv.FormatInt((20*time.Minute).Milliseconds(), 10), rec["ttl"], "HSET 的 ttl 必须是 eff")
				fx.wantUntil(20*time.Minute, "HSET 的 until 必须是本次截止")
				fx.wantPTTL(20*time.Minute, "PEXPIRE 必须用同一个 eff")
				score, err := fx.c.ZScore(fx.ctx, healthActiveZSet, fx.field).Result()
				require.NoError(fx.t, err)
				require.Equal(fx.t, float64(fx.gen), score, "ZADD 的分值 = 最新 generation")
				tomb, err := fx.c.HExists(fx.ctx, healthTombstoneHash, fx.field).Result()
				require.NoError(fx.t, err)
				require.False(fx.t, tomb, "HDEL 必须清掉墓碑哈希条目（否则 Sync 会把旧记录装回来）")
				exists, err := fx.c.Exists(fx.ctx, healthTombstonePrefix+fx.field).Result()
				require.NoError(fx.t, err)
				require.Zero(fx.t, exists, "DEL 必须清掉墓碑前缀键")

				// rev 栅栏的反向证明：旧 generation 必须被 readyLua 拒绝且不留副作用。
				_, err = fx.h.MarkReady(fx.ctx, fx.key, fx.gen-1, time.Minute)
				require.Error(fx.t, err, "过期 generation 必须被 readyLua 拒绝")
				require.Equal(fx.t, "1", fx.record()["rev"], "拒绝路径不得改动记录")
				// 正向：gen 与 rev 都对得上才删记录。若 HSET 漏写 rev（还是 9）这里必红。
				got, err := fx.h.MarkReady(fx.ctx, fx.key, fx.gen, time.Minute)
				require.NoError(fx.t, err, "gen+rev 都对上时 readyLua 必须成功")
				require.Greater(fx.t, got, fx.gen)
				existed, err := fx.c.Exists(fx.ctx, healthRecordPrefix+fx.field).Result()
				require.NoError(fx.t, err)
				require.Zero(fx.t, existed, "READY 之后记录消失")
			},
		},
		{
			// spec 行 8 的 latch 面：空库可建 + 成功返回 gen>0。
			name:  "空库可建",
			seed:  func(string) *cooldownSeed { return nil },
			only:  cooldownColLatch,
			until: 5 * time.Minute,
			why: "「可建」是 latch 面专属语义（Throttle/ThrottleUntil 可建可改）；补拉面在空库上" +
				"必须拒绝，见「空库拒绝」行——对 extend 断言「建成了」等于把闸门失效当成通过。",
			check: func(fx cooldownFixture) {
				fx.mustSucceed()
				require.Equal(fx.t, "RETRY_AFTER", fx.record()["state"])
				fx.wantUntil(5*time.Minute, "until 取本次截止")
				fx.wantPTTL(5*time.Minute, "空库建记录时 eff = until-now")
				require.Equal(fx.t, "1", fx.record()["rev"])
			},
		},
		{
			// spec 行 8 的 extend 面：拒绝返回 0，且零写副作用。
			name:  "空库拒绝",
			seed:  func(string) *cooldownSeed { return nil },
			only:  cooldownColExtend,
			until: 5 * time.Minute,
			why: "拒绝语义只属于补拉面的三闸（存在 / 同 state / 未过截止）；latch 面可建可改，" +
				"空库上永远返回 gen>0，给它断「返回 0」是错的。",
			check: func(fx cooldownFixture) {
				require.NoError(fx.t, fx.err, "拒绝是 (0,nil)，不是错误")
				require.Zero(fx.t, fx.gen, "拒绝必须返回 0，否则调用方会把它计成延长成功")
				exists, err := fx.c.Exists(fx.ctx, healthRecordPrefix+fx.field).Result()
				require.NoError(fx.t, err)
				require.Zero(fx.t, exists, "拒绝不得新建记录")
				require.Zero(fx.t, fx.c.ZCard(fx.ctx, healthActiveZSet).Val(), "拒绝不得写活动 ZSET")
				require.Equal(fx.t, fx.gen0, fx.genKeyValue(), "拒绝不得推进 generation")
			},
		},
	}

	ran := map[string]int{}
	for _, tc := range cases {
		tc := tc
		if tc.only != "" {
			require.NotEmpty(t, tc.why, "%s：只跑一列的行必须写明理由", tc.name)
			require.Contains(t, []string{cooldownColLatch, cooldownColExtend}, tc.only, "%s：only 必须是列名", tc.name)
		}
		for _, col := range cooldownColumns() {
			col := col
			if tc.only != "" && tc.only != col.name {
				continue
			}
			ran[col.name]++
			t.Run(tc.name+"/"+col.name, func(t *testing.T) {
				runCooldownWriteCase(t, tc, col)
			})
		}
	}
	// 两列都必须被绝大多数行跑到：只剩一两行时「镜像」就是空话。
	require.GreaterOrEqual(t, ran[cooldownColLatch], 7, "latch 列的覆盖行数")
	require.GreaterOrEqual(t, ran[cooldownColExtend], 7, "extend 列的覆盖行数")
}

func runCooldownWriteCase(t *testing.T, tc cooldownWriteCase, col cooldownColumn) {
	t.Helper()
	_, c := newHealthTestRedis(t)
	h, now := newFixedClockHealth(t, c, "self-a")
	ctx := context.Background()
	key := healthKeyFor(90, "q1", 1) // 每个子测试各自一套 miniredis，账号号段无需错开
	field := key.String()

	seed := tc.seed(col.name)
	if col.name == cooldownColExtend && seed != nil {
		// 「extend 列的前置记录必须是 RETRY_AFTER 且未过截止」——写成机械校验，
		// 否则某行悄悄被三闸拒绝，共享核心一行都没跑，断言照样绿。
		require.Equal(t, "RETRY_AFTER", seed.state, "extend 列的前置记录必须是 RETRY_AFTER")
		require.Greater(t, seed.pttl, time.Duration(0), "extend 列的前置记录不能已过期")
		if seed.until != nil && seed.untilRaw == "" {
			require.Greater(t, *seed.until, time.Duration(0), "extend 列的前置截止不能已过")
		}
	}
	seed.apply(t, ctx, c, field, *now)

	gen0 := c.Get(ctx, healthGenKey).Val()
	gen, err := col.write(h, key, now.Add(tc.until))
	if !col.canCreate && seed == nil {
		// 补拉面在空库上只可能拒绝：万一三闸失效走成了 latch 语义，这里先红。
		require.Zero(t, gen, "extend 面空库必须拒绝（canCreate=false）")
	}
	tc.check(cooldownFixture{
		t:     t,
		ctx:   ctx,
		c:     c,
		h:     h,
		col:   col,
		key:   key,
		field: field,
		now:   *now,
		seed:  seed,
		gen:   gen,
		gen0:  gen0,
		err:   err,
	})
}

// extendOnlyGate 是 extendCooldownLua 相对 throttleLua 的**唯一**代码差异。它被
// 逐字抄在这里，于是「三闸之外还有没有别的分叉」不需要靠人眼比对。
const extendOnlyGate = `-- 补拉三闸（相对 throttleLua 唯一的代码差异）：必须放在 INCR 之前——拒绝路径
-- （返回 0）不得产生任何写副作用。只看「是否存在 + 状态是否相同 + 是否已过截止」，
-- 正是补拉与运维恢复/其他失败类别/自然到期之间的边界；latch 面不做此检查（可建、可改）。
-- EXISTS 与下一行的 state 闸门对「记录不存在」等价（HGET 返回 false）；保留它是把「不存在」显式化。
if redis.call('EXISTS', recKey) == 0 then return 0 end
if redis.call('HGET', recKey, 'state') ~= state then return 0 end
if oldUntil > 0 and oldUntil <= nowMs then return 0 end
`

// TestCooldownScriptsShareVerbatimCore 机械证明「共享核心逐字相同」：从 ARGV 契约
// 行开始（含 KEYS/ARGV 绑定、31d 双夹、until 规范化、只延长、六步写入、return gen），
// 把 extend 侧那一段三闸摘掉之后，必须与 latch 侧全文一个字都不差。
//
// 为什么这条必须是文本断言：momus Q3 的清单里，「ARGV 绑定顺序」「注释里的口径」
// 这类分叉在行为上不可观察——两侧各自跑绿，安全不变量却已经分家。文本相等是唯一
// 能机械守住它的断言。
func TestCooldownScriptsShareVerbatimCore(t *testing.T) {
	const coreStart = "-- ARGV[1]=field"
	require.Equal(t, 1, strings.Count(throttleLua, coreStart), "latch 脚本必须只有一处 ARGV 契约行")
	require.Equal(t, 1, strings.Count(extendCooldownLua, coreStart), "extend 脚本必须只有一处 ARGV 契约行")

	latchCore := throttleLua[strings.Index(throttleLua, coreStart):]
	extendCore := extendCooldownLua[strings.Index(extendCooldownLua, coreStart):]

	require.Equal(t, 1, strings.Count(extendCore, extendOnlyGate), "三闸必须逐字出现且只出现一次")
	require.Equal(t, latchCore, strings.Replace(extendCore, extendOnlyGate, "", 1),
		"两脚本共享核心必须逐字相同（注释与空格也算）；唯一允许的分叉就是三闸")

	// 两脚本的 ARGV 个数必须是 7（第 8 个实参是 mode 的老形态已经删除）。
	require.NotContains(t, throttleLua+extendCooldownLua, "ARGV[8]", "旧 ARGV[8]=mode 分派不得残留")
	require.NotContains(t, throttleLua+extendCooldownLua, "loadstring", "禁止运行时拼脚本")
	require.NotContains(t, throttleLua, "mode", "latch 脚本不得残留 mode 分派")
}

// TestCooldownScriptsWriteStepOrder 钉六步写入的相对顺序。行为断言只能验证「六步
// 都发生了」（见镜像表的「六步写入顺序」行），顺序本身不可观察；而 ZADD 与
// HDEL/DEL 掉序正是「tombstone 与新记录交错」的来源，所以另用结构断言兜住。
func TestCooldownScriptsWriteStepOrder(t *testing.T) {
	steps := []string{
		"redis.call('INCR', genKey)",
		"redis.call('HSET', recKey,",
		"redis.call('PEXPIRE', recKey, eff)",
		"redis.call('ZADD', activeKey, gen, field)",
		"redis.call('HDEL', tombHash, field)",
		"redis.call('DEL', tombPrefix .. field)",
		"return gen",
	}
	scripts := []struct {
		name string
		body string
	}{
		{name: "throttleLua", body: throttleLua},
		{name: "extendCooldownLua", body: extendCooldownLua},
	}
	for _, s := range scripts {
		s := s
		t.Run(s.name, func(t *testing.T) {
			prev := -1
			for _, step := range steps {
				require.Equal(t, 1, strings.Count(s.body, step), "%s 必须出现且只出现一次：%s", s.name, step)
				at := strings.Index(s.body, step)
				require.Greater(t, at, prev, "%s 的写入顺序被改动：%s 必须在上一步之后", s.name, step)
				prev = at
			}
			// 三闸（若存在）必须整体早于 INCR：拒绝路径不得产生写副作用。
			if gateAt := strings.Index(s.body, "if redis.call('EXISTS', recKey) == 0 then return 0 end"); gateAt >= 0 {
				require.Less(t, gateAt, strings.Index(s.body, steps[0]), "闸门必须在 INCR 之前")
			}
		})
	}
}
