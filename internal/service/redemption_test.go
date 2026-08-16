// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// codeRe 兑换码格式：XXXX-XXXX-XXXX-XXXX，字符集大写 A-Z 去 I/O + 数字 2-9 去 0/1。
var codeRe = regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$`)

// newRedemptionSvc 构造带记录 inv 的 Service（兑换成功后必须刷新 auth 快照）。
func newRedemptionSvc() (*Service, *fakeStore, *invRecorder) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	return svc, fs, rec
}

// seedUser 建用户（Balance/MaxConcurrency 可预置）。
func seedUser(t *testing.T, fs *fakeStore, email string, balance int64, maxConcurrency int) *domain.User {
	t.Helper()
	u, err := fs.CreateUser(context.Background(), &domain.User{
		Email: email, Role: domain.RoleUser, Status: domain.UserStatusActive,
		Balance: balance, MaxConcurrency: maxConcurrency,
	})
	require.NoError(t, err)
	return u
}

// genOne 生成单码并回读落库版本（GenerateCodes 返回的 slice 无 ID——真实 repo
// CreateBulk 亦不填充；需要 ID 的断言一律回读）。
func genOne(t *testing.T, svc *Service, req GenerateRequest, createdBy int64) *domain.RedemptionCode {
	t.Helper()
	codes, err := svc.GenerateCodes(context.Background(), req, createdBy)
	require.NoError(t, err)
	require.Len(t, codes, 1)
	got, err := svc.store.GetByCode(context.Background(), codes[0].Code)
	require.NoError(t, err)
	return got
}

// TestGenerateCodes 生成校验（各字段非法 → 400）+ 批量 count + 格式/字符集/唯一
// + temp_balance 必填 resource_expires_at + created_by 落库 + 冲突重试终止（N=5）。
func TestGenerateCodes(t *testing.T) {
	svc, fs, _ := newRedemptionSvc()
	ctx := context.Background()

	t.Run("非法参数 400", func(t *testing.T) {
		bad := []GenerateRequest{
			{Type: domain.RedemptionType("bogus"), Value: 100},                      // type 非法
			{Type: domain.RedemptionTypeBalance, Value: 0},                          // value = 0
			{Type: domain.RedemptionTypeBalance, Value: -1},                         // value < 0
			{Type: domain.RedemptionTypeBalance, Value: 100, MaxUses: -1},           // max_uses < 0
			{Type: domain.RedemptionTypeBalance, Value: 100, Count: -1},             // count < 0
			{Type: domain.RedemptionTypeBalance, Value: 100, Count: 1001},           // count > 1000
			{Type: domain.RedemptionTypeBalance, Value: 100, ExpiresAt: &time.Time{}}, // expires_at 过去
			{Type: domain.RedemptionTypeTempBalance, Value: 100},                    // temp_balance 缺 resource_expires_at
			{Type: domain.RedemptionTypeTempBalance, Value: 100, ResourceExpiresAt: &time.Time{}}, // resource_expires_at 过去
		}
		for _, req := range bad {
			_, err := svc.GenerateCodes(ctx, req, 0)
			require.ErrorIs(t, err, ErrInvalidInput, "req=%+v", req)
		}
	})

	t.Run("批量 count 50：格式/字符集/唯一/默认值", func(t *testing.T) {
		remark := "运营"
		codes, err := svc.GenerateCodes(ctx, GenerateRequest{
			Type: domain.RedemptionTypeBalance, Value: 500, Remark: &remark, Count: 50,
		}, 0)
		require.NoError(t, err)
		require.Len(t, codes, 50)
		seen := make(map[string]bool, 50)
		for _, c := range codes {
			require.Regexp(t, codeRe, c.Code, "格式 XXXX-XXXX-XXXX-XXXX + 字符集（A-Z 去 I/O + 2-9 去 0/1）")
			require.False(t, seen[c.Code], "码必须唯一")
			seen[c.Code] = true
			require.Equal(t, domain.RedemptionStatusActive, c.Status)
			require.Equal(t, 1, c.MaxUses, "max_uses 未提供 → 默认 1（决策 3）")
			require.Equal(t, int64(500), c.Value)
			require.Equal(t, "运营", *c.Remark)
		}
		got, err := fs.GetByCode(ctx, codes[0].Code)
		require.NoError(t, err)
		require.Equal(t, int64(500), got.Value, "批量插入全部落库")
	})

	t.Run("count 上限 1000 与 max_uses 显式", func(t *testing.T) {
		codes, err := svc.GenerateCodes(ctx, GenerateRequest{
			Type: domain.RedemptionTypeConcurrency, Value: 5, MaxUses: 3, Count: 1000,
		}, 0)
		require.NoError(t, err)
		require.Len(t, codes, 1000)
		seen := make(map[string]bool, 1000)
		for _, c := range codes {
			require.False(t, seen[c.Code], "1000 批量内码必须唯一（80bit 熵碰撞概率 ~0）")
			seen[c.Code] = true
		}
		got, err := fs.GetByCode(ctx, codes[0].Code)
		require.NoError(t, err)
		require.Equal(t, 3, got.MaxUses, "显式 max_uses 落库")
	})

	t.Run("temp_balance 全字段落库", func(t *testing.T) {
		remark := "试用"
		exp := time.Now().Add(24 * time.Hour)
		re := time.Now().Add(7 * 24 * time.Hour)
		codes, err := svc.GenerateCodes(ctx, GenerateRequest{
			Type: domain.RedemptionTypeTempBalance, Value: 1000, Remark: &remark,
			ExpiresAt: &exp, ResourceExpiresAt: &re,
		}, 42)
		require.NoError(t, err)
		got, err := fs.GetByCode(ctx, codes[0].Code)
		require.NoError(t, err)
		require.Equal(t, domain.RedemptionTypeTempBalance, got.Type)
		require.Equal(t, "试用", *got.Remark)
		require.NotNil(t, got.ExpiresAt)
		require.NotNil(t, got.ResourceExpiresAt)
		require.Equal(t, re, *got.ResourceExpiresAt)
	})

	t.Run("created_by 0 与 >0 都落库（决策 5）", func(t *testing.T) {
		c0, err := svc.GenerateCodes(ctx, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 10}, 0)
		require.NoError(t, err)
		g0, err := fs.GetByCode(ctx, c0[0].Code)
		require.NoError(t, err)
		require.Zero(t, g0.CreatedBy, "0 = 系统（静态 admin token）")

		c42, err := svc.GenerateCodes(ctx, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 10}, 42)
		require.NoError(t, err)
		g42, err := fs.GetByCode(ctx, c42[0].Code)
		require.NoError(t, err)
		require.Equal(t, int64(42), g42.CreatedBy, ">0 = platform_admin user_id")
	})

	t.Run("code 唯一冲突重试 N=5 后终止", func(t *testing.T) {
		fs.codesConflictAlways = true
		_, err := svc.GenerateCodes(ctx, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 10}, 0)
		require.ErrorIs(t, err, ErrConflict, "5 次重试全冲突 → 409（碰撞概率 ~0 的兜底）")
		fs.codesConflictAlways = false
	})
}

// TestListCodes 列表：非法枚举/sort/order → 400；type/status 筛选。
func TestListCodes(t *testing.T) {
	svc, _, _ := newRedemptionSvc()
	ctx := context.Background()

	genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
	genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeConcurrency, Value: 5}, 0)
	cb := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 200}, 0)
	require.NoError(t, svc.DeactivateCode(ctx, cb.ID))

	bt := domain.RedemptionType("bogus")
	_, _, err := svc.ListCodes(ctx, repository.ListQuery{}, &bt, nil)
	require.ErrorIs(t, err, ErrInvalidInput, "type 非法枚举 → 400")
	st := domain.RedemptionStatus("bogus")
	_, _, err = svc.ListCodes(ctx, repository.ListQuery{}, nil, &st)
	require.ErrorIs(t, err, ErrInvalidInput, "status 非法枚举 → 400")
	_, _, err = svc.ListCodes(ctx, repository.ListQuery{Sort: "hacked"}, nil, nil)
	require.ErrorIs(t, err, ErrInvalidInput, "sort 白名单外 → 400")
	_, _, err = svc.ListCodes(ctx, repository.ListQuery{Order: "sideways"}, nil, nil)
	require.ErrorIs(t, err, ErrInvalidInput, "order 非法 → 400")

	rows, total, err := svc.ListCodes(ctx, repository.ListQuery{}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 3)

	ab := domain.RedemptionTypeBalance
	rows, total, err = svc.ListCodes(ctx, repository.ListQuery{}, &ab, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	for _, c := range rows {
		require.Equal(t, domain.RedemptionTypeBalance, c.Type)
	}

	as := domain.RedemptionStatusActive
	rows, total, err = svc.ListCodes(ctx, repository.ListQuery{}, &ab, &as)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, domain.RedemptionStatusActive, rows[0].Status)
}

// TestGetCodeUses 审计：码不存在 → 404；兑换后记录可查（值快照 + 资源到期）。
func TestGetCodeUses(t *testing.T) {
	svc, fs, _ := newRedemptionSvc()
	ctx := context.Background()

	_, _, err := svc.GetCodeUses(ctx, 99999, repository.ListQuery{})
	require.ErrorIs(t, err, ErrNotFound, "码不存在 → 404")

	c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
	uses, total, err := svc.GetCodeUses(ctx, c.ID, repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, uses)

	u := seedUser(t, fs, "uses@example.com", 0, 0)
	_, err = svc.Redeem(ctx, c.Code, u.ID)
	require.NoError(t, err)
	uses, total, err = svc.GetCodeUses(ctx, c.ID, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, uses, 1)
	require.Equal(t, u.ID, uses[0].UserID)
	require.Equal(t, int64(100), uses[0].Value, "兑换时的值快照")
	require.Nil(t, uses[0].ResourceExpiresAt, "balance 码资源无到期 → nil")

	// 分页（spec 2026-08-17 补参数）：25 个不同用户各兑一次 → 25 条；limit/offset
	// 翻页取全（此前 GetCodeUses 空 query 恒 20 行截断、Total 失真；同一用户
	// 复兑同一码 → 409 已兑换，见评审 M-1）。
	gen := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100, MaxUses: 100}, 0)
	for i := 0; i < 25; i++ {
		u2 := seedUser(t, fs, fmt.Sprintf("p%d@example.com", i), 0, 0)
		_, err = svc.Redeem(ctx, gen.Code, u2.ID)
		require.NoError(t, err)
	}
	uses, total, err = svc.GetCodeUses(ctx, gen.ID, repository.ListQuery{Limit: 10, Offset: 20})
	require.NoError(t, err)
	require.Equal(t, int64(25), total, "Total 恒全量（不随分页裁剪）")
	require.Len(t, uses, 5, "limit=10&offset=20 → 尾页 5 条（此前恒 20 行不可翻页）")
}

// TestDeactivateCode 单码失效：缺失 → 404 含详情；已 disabled → no-op 成功。
func TestDeactivateCode(t *testing.T) {
	svc, fs, _ := newRedemptionSvc()
	ctx := context.Background()

	err := svc.DeactivateCode(ctx, 99999)
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "id=99999 missing")

	c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
	require.NoError(t, svc.DeactivateCode(ctx, c.ID))
	got, err := fs.GetCode(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, domain.RedemptionStatusDisabled, got.Status)

	require.NoError(t, svc.DeactivateCode(ctx, c.ID), "已 disabled → no-op 成功（幂等重放友好）")
}

// TestDeactivateCodesBatch 批量失效（决策 6）：validateIDs 400；缺失 id → 404
// 含详情且先查后失效（有效 id 不受影响）；no-op 不计受影响数；成功计数。
func TestDeactivateCodesBatch(t *testing.T) {
	svc, fs, _ := newRedemptionSvc()
	ctx := context.Background()

	_, err := svc.DeactivateCodesBatch(ctx, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.DeactivateCodesBatch(ctx, []int64{1, 1})
	require.ErrorIs(t, err, ErrInvalidInput, "重复 id → 400")
	ids101 := make([]int64, 101)
	for i := range ids101 {
		ids101[i] = int64(i + 1)
	}
	_, err = svc.DeactivateCodesBatch(ctx, ids101)
	require.ErrorIs(t, err, ErrInvalidInput, ">100 条 → 400")

	c1 := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
	c2 := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 200}, 0)
	require.NoError(t, svc.DeactivateCode(ctx, c1.ID))

	_, err = svc.DeactivateCodesBatch(ctx, []int64{c1.ID, c2.ID, 99999})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "id=99999 missing")
	got, err := fs.GetCode(ctx, c2.ID)
	require.NoError(t, err)
	require.Equal(t, domain.RedemptionStatusActive, got.Status, "先查后失效：404 时任何 id 都不被失效")

	n, err := svc.DeactivateCodesBatch(ctx, []int64{c1.ID, c2.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "c1 已 disabled no-op，仅 c2 新失效")
	for _, id := range []int64{c1.ID, c2.ID} {
		got, err := fs.GetCode(ctx, id)
		require.NoError(t, err)
		require.Equal(t, domain.RedemptionStatusDisabled, got.Status)
	}

	n, err = svc.DeactivateCodesBatch(ctx, []int64{c1.ID, c2.ID})
	require.NoError(t, err)
	require.Zero(t, n, "全 disabled → 0，无错（幂等重放友好）")
}

// TestRedeem 兑换成功（三类型）+ 错误语义（409/400）+ invalidate 时机。
func TestRedeem(t *testing.T) {
	svc, fs, rec := newRedemptionSvc()
	ctx := context.Background()

	t.Run("balance 加值 + invalidate", func(t *testing.T) {
		u := seedUser(t, fs, "bal@example.com", 0, 0)
		c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 500}, 0)
		before := rec.total()
		apply, err := svc.Redeem(ctx, c.Code, u.ID)
		require.NoError(t, err)
		require.Equal(t, domain.RedemptionTypeBalance, apply.Type)
		require.Equal(t, int64(500), apply.Value)
		require.Nil(t, apply.ResourceExpiresAt)
		got, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(500), got.Balance, "余额 += value")
		require.Greater(t, rec.total(), before, "兑换成功后必须 invalidate（决策 8：auth 快照刷新）")
		stored, err := fs.GetByCode(ctx, c.Code)
		require.NoError(t, err)
		require.Equal(t, 1, stored.UsedCount, "used_count 条件递增")
	})

	t.Run("concurrency 0 → value / 100 → 100+value（决策 2）", func(t *testing.T) {
		u0 := seedUser(t, fs, "con0@example.com", 0, 0)
		c0 := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeConcurrency, Value: 5}, 0)
		_, err := svc.Redeem(ctx, c0.Code, u0.ID)
		require.NoError(t, err)
		got, err := fs.GetUser(ctx, u0.ID)
		require.NoError(t, err)
		require.Equal(t, 5, got.MaxConcurrency, "0 = 不限 → 直接设为 value")

		u1 := seedUser(t, fs, "con100@example.com", 0, 100)
		c1 := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeConcurrency, Value: 5}, 0)
		_, err = svc.Redeem(ctx, c1.Code, u1.ID)
		require.NoError(t, err)
		got, err = fs.GetUser(ctx, u1.ID)
		require.NoError(t, err)
		require.Equal(t, 105, got.MaxConcurrency, "非 0 → 累加")
	})

	t.Run("temp_balance 插行（含到期与 note）", func(t *testing.T) {
		u := seedUser(t, fs, "tmp@example.com", 0, 0)
		re := time.Now().Add(7 * 24 * time.Hour)
		c := genOne(t, svc, GenerateRequest{
			Type: domain.RedemptionTypeTempBalance, Value: 1000, ResourceExpiresAt: &re,
		}, 0)
		apply, err := svc.Redeem(ctx, c.Code, u.ID)
		require.NoError(t, err)
		require.Equal(t, domain.RedemptionTypeTempBalance, apply.Type)
		require.Equal(t, int64(1000), apply.Value)
		require.NotNil(t, apply.ResourceExpiresAt)
		require.Equal(t, re, *apply.ResourceExpiresAt, "回执携带资源到期")
		require.Len(t, fs.temps, 1, "temp_balance 行已插入")
		row := fs.temps[0]
		require.Equal(t, u.ID, row.UserID)
		require.Equal(t, int64(1000), row.Amount)
		require.NotNil(t, row.ExpiresAt)
		require.Equal(t, re, *row.ExpiresAt, "expires_at = resource_expires_at")
		require.NotNil(t, row.Note)
		require.Equal(t, "redemption code", *row.Note)
		got, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Zero(t, got.Balance, "temp_balance 不动 users.balance")
		require.Zero(t, got.MaxConcurrency)
	})

	t.Run("重复兑换 409（先查 use，评审 M-1）", func(t *testing.T) {
		u := seedUser(t, fs, "dup@example.com", 0, 0)
		c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
		_, err := svc.Redeem(ctx, c.Code, u.ID)
		require.NoError(t, err)
		before := rec.total()
		_, err = svc.Redeem(ctx, c.Code, u.ID)
		require.ErrorIs(t, err, ErrConflict)
		require.Contains(t, err.Error(), "already redeemed")
		require.Equal(t, before, rec.total(), "失败路径不 invalidate")
		got, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(100), got.Balance, "重复兑换余额不变")
	})

	t.Run("已失效码的重复兑换仍 409（评审 M-1）", func(t *testing.T) {
		u := seedUser(t, fs, "dupdis@example.com", 0, 0)
		c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
		_, err := svc.Redeem(ctx, c.Code, u.ID)
		require.NoError(t, err)
		require.NoError(t, svc.DeactivateCode(ctx, c.ID))
		_, err = svc.Redeem(ctx, c.Code, u.ID)
		require.ErrorIs(t, err, ErrConflict, "先查 use：已兑换事实优先于码状态")
	})

	t.Run("用尽 400 + 回滚（评审 I-1/I-2）", func(t *testing.T) {
		c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0) // max_uses 1
		u1 := seedUser(t, fs, "exhaust1@example.com", 0, 0)
		u2 := seedUser(t, fs, "exhaust2@example.com", 0, 0)
		_, err := svc.Redeem(ctx, c.Code, u1.ID)
		require.NoError(t, err)
		_, err = svc.Redeem(ctx, c.Code, u2.ID)
		require.ErrorIs(t, err, ErrInvalidInput, "用尽 → 400 invalid code")
		require.Contains(t, err.Error(), "invalid code")
		got, err := fs.GetUser(ctx, u2.ID)
		require.NoError(t, err)
		require.Zero(t, got.Balance, "IncrementUsed false → 整体回滚：余额不变")
	})

	t.Run("失效/过期/不存在码 → 400 invalid code（统一不泄露）", func(t *testing.T) {
		u := seedUser(t, fs, "bad@example.com", 0, 0)
		c1 := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
		require.NoError(t, svc.DeactivateCode(ctx, c1.ID))
		_, err := svc.Redeem(ctx, c1.Code, u.ID)
		require.ErrorIs(t, err, ErrInvalidInput, "失效码 → 400")
		require.Contains(t, err.Error(), "invalid code")

		// 过期码不能经 GenerateCodes 造（生成时 expires_at 必须 > now），直接 repo 层插入
		past := time.Now().Add(-time.Hour)
		expired := &domain.RedemptionCode{
			Code: "EXPIRED-0001", Type: domain.RedemptionTypeBalance, Value: 100,
			ExpiresAt: &past, MaxUses: 1, Status: domain.RedemptionStatusActive,
		}
		require.NoError(t, fs.CreateCodes(ctx, []*domain.RedemptionCode{expired}))
		_, err = svc.Redeem(ctx, expired.Code, u.ID)
		require.ErrorIs(t, err, ErrInvalidInput, "过期码 → 400")

		_, err = svc.Redeem(ctx, "ZZZZZZ-ZZZZZZ", u.ID)
		require.ErrorIs(t, err, ErrInvalidInput, "不存在码 → 400")
		got, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Zero(t, got.Balance, "全部 400 路径无资源变更")
	})
}

// TestRedeemRollback 回滚断言（评审 I-1）：WithTx 内任一步失败 → 暂存变更
// 全部丢弃——use 唯一冲突（并发窗口：另一事务已提交同 use）/ IncrementUsed
// 用尽 → 余额/并发不变。
func TestRedeemRollback(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()

	t.Run("use 唯一冲突", func(t *testing.T) {
		u := seedUser(t, fs, "rollback@example.com", 100, 10)
		code := &domain.RedemptionCode{
			Code: "ROLLBK-0001", Type: domain.RedemptionTypeBalance, Value: 100,
			MaxUses: 1, Status: domain.RedemptionStatusActive,
		}
		require.NoError(t, fs.CreateCodes(ctx, []*domain.RedemptionCode{code}))
		stored, err := fs.GetByCode(ctx, code.Code)
		require.NoError(t, err)
		// 并发窗口：另一事务已提交 (code_id, user_id) use——本事务 GetUse 前置
		// 检查不可能拦住（读的是提交前视图），CreateUse 唯一约束兜底 409
		require.NoError(t, fs.CreateUse(ctx, &domain.RedemptionUse{CodeID: stored.ID, UserID: u.ID, Value: 100}))

		err = fs.WithTx(ctx, func(tx repository.TxStore) error {
			require.NoError(t, tx.UpdateUserBalance(ctx, u.ID, 100))
			require.NoError(t, tx.UpdateUserMaxConcurrency(ctx, u.ID, 5))
			err := tx.CreateUse(ctx, &domain.RedemptionUse{CodeID: stored.ID, UserID: u.ID, Value: 100})
			require.ErrorIs(t, err, repository.ErrConflict, "唯一冲突 → 409 语义")
			return err
		})
		require.Error(t, err)

		got, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(100), got.Balance, "use 冲突回滚：余额不变")
		require.Equal(t, 10, got.MaxConcurrency, "use 冲突回滚：并发不变")
		uses, total, err := fs.ListCodeUses(ctx, stored.ID, repository.ListQuery{})
		require.NoError(t, err)
		require.Equal(t, int64(1), total, "无重复 use 残留")
		require.Len(t, uses, 1)
	})

	t.Run("IncrementUsed false（用尽）", func(t *testing.T) {
		u := seedUser(t, fs, "incr@example.com", 0, 0)
		code := &domain.RedemptionCode{
			Code: "EXHAUST-01", Type: domain.RedemptionTypeConcurrency, Value: 5,
			MaxUses: 1, UsedCount: 1, Status: domain.RedemptionStatusActive, // 已用尽
		}
		require.NoError(t, fs.CreateCodes(ctx, []*domain.RedemptionCode{code}))
		stored, err := fs.GetByCode(ctx, code.Code)
		require.NoError(t, err)

		err = fs.WithTx(ctx, func(tx repository.TxStore) error {
			require.NoError(t, tx.UpdateUserMaxConcurrency(ctx, u.ID, 5))
			require.NoError(t, tx.UpdateUserBalance(ctx, u.ID, 100))
			ok, err := tx.IncrementUsed(ctx, stored.ID)
			require.NoError(t, err)
			require.False(t, ok, "used_count >= max_uses → (false, nil)")
			return fmt.Errorf("%w: invalid code", ErrInvalidInput)
		})
		require.Error(t, err)

		got, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Zero(t, got.Balance, "用尽回滚：余额不变")
		require.Zero(t, got.MaxConcurrency, "用尽回滚：并发不变")
		still, err := fs.GetByCode(ctx, code.Code)
		require.NoError(t, err)
		require.Equal(t, 1, still.UsedCount, "used_count 未被改动")
	})
}
