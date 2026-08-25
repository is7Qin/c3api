// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package verification

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/redisx"
)

// 编译期锚：*Store 必须持续满足 service.EmailCodeStore 四方法面（spec §2 映射表）。
var _ service.EmailCodeStore = (*Store)(nil)

// newTestStore miniredis + redisx.Open（单点构造纪律）注入；返回 store 与
// miniredis 句柄（FastForward/TTL 虚拟时钟断言用）。无 sleep——时间推进全部走
// miniredis 虚拟时钟。
func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	return New(c), mr
}

const (
	testEmail   = "user@example.com"
	testPurpose = "register"
	testSHA     = "aabb"
	testSHA2    = "ccdd"
)

// TestUpsertGetRoundTrip_whenFreshKey 场景 §3.1：Upsert→Get 往返四字段全等
// （含 updated_at 毫秒级）；键名与 HASH 形状（恰好三字段）符合 spec §2。
func TestUpsertGetRoundTrip_whenFreshKey(t *testing.T) {
	st, mr := newTestStore(t)
	ctx := context.Background()
	expires := time.Now().Add(domain.EmailCodeTTL)

	upserted, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, expires)
	require.NoError(t, err)

	k := "c3api:emailcode:" + testPurpose + ":" + testEmail
	require.True(t, mr.Exists(k), "键名 c3api:emailcode:<purpose>:<email>")

	got, err := st.GetEmailCode(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, testEmail, got.Email)
	require.Equal(t, domain.EmailCodePurpose(testPurpose), got.Purpose)
	require.Equal(t, testSHA, got.CodeSHA256)
	require.Equal(t, 0, got.Attempts)
	// updated_at 毫秒级全等：读回值即 Upsert 写入的同一 unixmilli 整数
	require.Equal(t, upserted.UpdatedAt.UnixMilli(), got.UpdatedAt.UnixMilli())
	// EXPIREAT 秒级粒度 ⇒ ExpiresAt 重构值为秒级截断近似（spec §2：TTL 取代列判定）
	require.WithinDuration(t, expires, got.ExpiresAt, time.Second)

	fields, err := mr.HKeys(k)
	require.NoError(t, err)
	require.Len(t, fields, 3, "HASH 恰含 code_sha256/attempts/updated_at 三字段")
}

// TestUpsertOverwriteResetsAttempts_whenResend 场景 §3.2：覆盖写归零——重发后
// attempts==0 且 code 为新值（旧码失效语义，对齐 PG 版整行覆盖）。
func TestUpsertOverwriteResetsAttempts_whenResend(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	expires := time.Now().Add(domain.EmailCodeTTL)

	_, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, expires)
	require.NoError(t, err)
	n, err := st.IncrementEmailCodeAttempts(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, err = st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA2, expires)
	require.NoError(t, err)

	got, err := st.GetEmailCode(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, testSHA2, got.CodeSHA256, "旧码被覆盖失效")
	require.Equal(t, 0, got.Attempts, "覆盖写 attempts 归零")
}

// TestGetNilAfterExpiry_whenFastForwardPastExpiresAt 场景 §3.3：FastForward 越
// 过 expires_at → 原生 TTL 生效，Get 返回 (nil,nil)。
func TestGetNilAfterExpiry_whenFastForwardPastExpiresAt(t *testing.T) {
	st, mr := newTestStore(t)
	ctx := context.Background()

	_, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, time.Now().Add(domain.EmailCodeTTL))
	require.NoError(t, err)

	mr.FastForward(domain.EmailCodeTTL + time.Second)

	got, err := st.GetEmailCode(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.Nil(t, got, "过期键已被 Redis 删除 → 服务层走 code invalid 分支")
}

// TestIncrementAttemptsReturnSequenceAndKeepTTL_whenMidWindow 场景 §3.4：
// HINCRBY 连增返回 1,2,3 且 TTL 不被触碰——半程 FastForward 后自增，剩余 TTL
// 保持半程值，继续推进仍按原过期时刻失效。
func TestIncrementAttemptsReturnSequenceAndKeepTTL_whenMidWindow(t *testing.T) {
	st, mr := newTestStore(t)
	ctx := context.Background()

	_, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, time.Now().Add(domain.EmailCodeTTL))
	require.NoError(t, err)

	mr.FastForward(domain.EmailCodeTTL / 2) // 半程

	for want := 1; want <= 3; want++ {
		n, err := st.IncrementEmailCodeAttempts(ctx, testEmail, testPurpose)
		require.NoError(t, err)
		require.Equal(t, want, n)
	}

	// EXPIREAT 秒级截断 ⇒ 剩余 TTL 为半程值（秒粒度内）；若 HINCRBY 误触 TTL
	// （清零或重置为全窗），下界/上界必炸——此即"自增不触碰 TTL"的断言形态。
	ttl := mr.TTL("c3api:emailcode:" + testPurpose + ":" + testEmail)
	require.Greater(t, ttl, domain.EmailCodeTTL/2-time.Second, "剩余有效期保持半程")
	require.LessOrEqual(t, ttl, domain.EmailCodeTTL/2, "未被延长")

	mr.FastForward(domain.EmailCodeTTL/2 + time.Second)
	got, err := st.GetEmailCode(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.Nil(t, got, "自增不延长有效期：继续推进仍过期")
}

// TestDeleteIdempotentAndNilTolerant_whenKeyMissing 场景 §3.5：Delete 后 Get=nil；
// DEL 不存在的键不报错。
func TestDeleteIdempotentAndNilTolerant_whenKeyMissing(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	_, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, time.Now().Add(domain.EmailCodeTTL))
	require.NoError(t, err)

	require.NoError(t, st.DeleteEmailCode(ctx, testEmail, testPurpose))
	got, err := st.GetEmailCode(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.Nil(t, got)

	require.NoError(t, st.DeleteEmailCode(ctx, testEmail, testPurpose), "DEL 缺键幂等不报错")
	require.NoError(t, st.DeleteEmailCode(ctx, "none@example.com", "reset"), "从未存在的键同理")
}

// TestUpdatedAtFreshnessForRateLimit_whenJustUpserted 场景 §3.6：Upsert 后立即
// Get → UpdatedAt 距今 <60s——SendRegisterCode 的 429 抑制判定输入成立。
func TestUpdatedAtFreshnessForRateLimit_whenJustUpserted(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	_, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, time.Now().Add(domain.EmailCodeTTL))
	require.NoError(t, err)

	got, err := st.GetEmailCode(ctx, testEmail, testPurpose)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Less(t, time.Since(got.UpdatedAt), domain.EmailCodeRateLimit,
		"updated_at 新鲜 → time.Since < EmailCodeRateLimit → 第二次发码命中 ErrTooManyRequests")
}

// TestGetNilOnRedisError_whenClientUnreachable 防御面冒烟：客户端故障时错误上抛
// 不吞（服务层 err!=nil 分支依赖此语义）。
func TestGetErrorSurfaces_whenClientClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	st := New(c)
	mr.Close() // 服务端先关

	_, err = st.GetEmailCode(context.Background(), testEmail, testPurpose)
	require.Error(t, err, "存储层错误必须上抛，不得伪装成 (nil,nil)")
}

// TestGetCorruptFieldError_whenRawHashMismatchesSchema 评审补充（2026-08-26
// Oracle 测试缺口）：绕过 Store 直写坏字段值 → GetEmailCode 上抛含定位信息的
// 包裹错误，不吞不 panic。
func TestGetCorruptFieldError_whenRawHashMismatchesSchema(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		raw     string
		wantErr string
	}{
		{"attempts 不是数字", "attempts", "abc", "corrupt attempts"},
		{"updated_at 不是毫秒整数", "updated_at", "not-a-number", "corrupt updated_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, mr := newTestStore(t)
			ctx := context.Background()

			_, err := st.UpsertEmailCode(ctx, testEmail, testPurpose, testSHA, time.Now().Add(domain.EmailCodeTTL))
			require.NoError(t, err)
			mr.HSet("c3api:emailcode:"+testPurpose+":"+testEmail, tc.field, tc.raw) // miniredis HSet 无返回值

			_, err = st.GetEmailCode(ctx, testEmail, testPurpose)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
