// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// GenerateRequest 兑换码生成参数（/api/admin/redemption-codes POST）。
// MaxUses ≤ 0 → 1（单次码，决策 3）；Count ≤ 0 → 1，上限 1000。
type GenerateRequest struct {
	Type              domain.RedemptionType
	Value             int64 // 最小单位（分 / 并发数）；> 0
	Remark            *string
	ExpiresAt         *time.Time // 码未兑换即过期；nil = 永久
	ResourceExpiresAt *time.Time // 兑换后资源到期；temp_balance 必填（决策 4）
	MaxUses           int
	Count             int
}

// validateGenerateRequest 生成参数校验（决策 4/契约）：type 枚举、value > 0、
// temp_balance 必填 resource_expires_at 且 > now、expires_at 若提供 > now、
// max_uses ≥ 1（0 = 默认）、count 1-1000（0 = 默认）。
func validateGenerateRequest(req GenerateRequest) error {
	if !req.Type.Valid() {
		return ErrInvalidInput
	}
	if req.Value <= 0 {
		return ErrInvalidInput
	}
	if req.MaxUses < 0 {
		return ErrInvalidInput
	}
	if req.Count < 0 || req.Count > 1000 {
		return ErrInvalidInput
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return ErrInvalidInput
	}
	if req.Type == domain.RedemptionTypeTempBalance {
		if req.ResourceExpiresAt == nil || !req.ResourceExpiresAt.After(time.Now()) {
			return ErrInvalidInput
		}
	}
	return nil
}

// codeCharset 兑换码字符集（32 字符）：大写 A-Z 去 I/O + 数字 2-9 去 0/1
// （防形似混淆）——16 字符熵 ~80bit（决策 3 数据模型；用户拍板升级：12→16 字符）。
const codeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// randomCode 生成单码（格式 XXXX-XXXX-XXXX-XXXX，crypto/rand 不可预测——码可兑换
// 真实资源，禁用可预测的 math/rand）。
func randomCode() (string, error) {
	b := make([]byte, 19)
	for i := 0; i < len(b); i++ {
		if i == 4 || i == 9 || i == 14 {
			b[i] = '-'
			continue
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeCharset))))
		if err != nil {
			return "", err
		}
		b[i] = codeCharset[n.Int64()]
	}
	return string(b), nil
}

// GenerateCodes 批量生成兑换码（1..1000）：校验 → 生成 count 个随机码 →
// CreateCodes 批量插入；code 唯一冲突（repository.ErrConflict）重试换新码
// （N=5，碰撞概率 ~0，兜底防御）。created_by 0 = 系统，>0 = platform_admin
// 用户 id（决策 5，无 NULL 分支）。
func (s *Service) GenerateCodes(ctx context.Context, req GenerateRequest, createdBy int64) ([]*domain.RedemptionCode, error) {
	if err := validateGenerateRequest(req); err != nil {
		return nil, err
	}
	maxUses := req.MaxUses
	if maxUses <= 0 {
		maxUses = 1 // 决策 3：max_uses 默认 1（单次码）
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}
	for attempt := 0; attempt < 5; attempt++ {
		codes := make([]*domain.RedemptionCode, 0, count)
		for i := 0; i < count; i++ {
			code, err := randomCode()
			if err != nil {
				return nil, err
			}
			codes = append(codes, &domain.RedemptionCode{
				Code: code, Type: req.Type, Value: req.Value,
				Remark: req.Remark, ExpiresAt: req.ExpiresAt,
				ResourceExpiresAt: req.ResourceExpiresAt,
				MaxUses:           maxUses, UsedCount: 0,
				Status: domain.RedemptionStatusActive, CreatedBy: createdBy,
			})
		}
		if err := s.store.CreateCodes(ctx, codes); err != nil {
			if errors.Is(err, repository.ErrConflict) && attempt < 4 {
				continue // code 唯一冲突 → 换新码重试（N=5）
			}
			return nil, mapRepoErr(err)
		}
		if s.log != nil {
			s.log.Info("redemption codes generated",
				logx.Int("count", count), logx.String("type", string(req.Type)),
				logx.Int64("created_by", createdBy))
		}
		return codes, nil
	}
	panic("unreachable") // 循环内 5 次迭代全部 return（continue 仅 attempt<4，末次冲突必 return）
}

// ListCodes 兑换码列表（/api/admin/redemption-codes）：type/status 筛选
// （nil = 不过滤；非法枚举 → 400）+ sort 白名单校验（非法 → 400）。
func (s *Service) ListCodes(ctx context.Context, q repository.ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error) {
	if typ != nil && !typ.Valid() {
		return nil, 0, ErrInvalidInput
	}
	if status != nil && !status.Valid() {
		return nil, 0, ErrInvalidInput
	}
	if err := validateListQuery(q, listSortFields["redemption_codes"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListCodes(ctx, q, typ, status)
}

// GetCode 按 id 取兑换码（只读访问器；缺失 → 404 含详情）。admin 审计端点
// /redemption-codes/{id}/uses 的面值换算需要码的 type（use 行不存类型），
// handler 边界先取码再换算——存储语义不变。
func (s *Service) GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error) {
	c, err := s.store.GetCode(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return c, nil
}

// GetCodeUses 某码的兑换记录（审计，/api/admin/redemption-codes/{id}/uses）：
// 码不存在 → 404（mapRepoErr 含详情）。use 快照不存码类型——handler 需先
// GetCode 取 type 做面值换算。q 直透 ListCodeUses（limit/offset 缺省归一在
// repo——≤0→20/<0→0；spec 2026-08-17 补分页参数）。
func (s *Service) GetCodeUses(ctx context.Context, codeID int64, q repository.ListQuery) ([]*domain.RedemptionUse, int64, error) {
	if _, err := s.store.GetCode(ctx, codeID); err != nil {
		return nil, 0, mapRepoErr(err)
	}
	return s.store.ListCodeUses(ctx, codeID, q)
}

// ListMyRedemptions 我的兑换记录（/api/user/redemptions）：use 快照 + 码的 type/remark
// 联查；userID 由 handler 从 JWT 取（强制本人数据，防越权）。sort/order 白名单
// 校验（非法 → 400）。
func (s *Service) ListMyRedemptions(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.RedemptionRecord, int64, error) {
	if err := validateListQuery(q, listSortFields["redemption_uses"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListUsesByUser(ctx, userID, q)
}

// DeactivateCode 单码失效（/api/admin/redemption-codes/{id}/deactivate）：
// 不存在 → 404 含详情；已 disabled → no-op 成功（幂等重放友好，决策 6）。
func (s *Service) DeactivateCode(ctx context.Context, id int64) error {
	c, err := s.store.GetCode(ctx, id)
	if err != nil {
		return mapRepoErr(err)
	}
	if c.Status == domain.RedemptionStatusDisabled {
		return nil // 已 disabled → no-op 成功
	}
	if _, err := s.store.DeactivateCodes(ctx, []int64{id}); err != nil {
		return mapRepoErr(err)
	}
	return nil
}

// DeactivateCodesBatch 批量失效（/api/admin/redemption-codes/batch-deactivate，
// 决策 6）：validateIDs → 逐 id 先查（缺失 id → 404 含缺失详情，对齐批量删除
// 范式）→ DeactivateCodes 单事务（已 disabled no-op）→ 返回新失效数。
// 先查后失效窗口竞态可接受：失效不新增行，检查到的 id 不会消失（评审 M-2）。
func (s *Service) DeactivateCodesBatch(ctx context.Context, ids []int64) (int64, error) {
	if err := validateIDs(ids); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.store.GetCode(ctx, id); err != nil {
			return 0, mapRepoErr(err) // 404 含缺失 id 详情
		}
	}
	return s.store.DeactivateCodes(ctx, ids)
}

// applyFunc 兑换资源应用函数：只经 tx 面（repository.TxStore）操作——评审 I-1：
// 任一步失败（含 use 冲突/计数用尽）整体回滚（余额/并发不变）。
type applyFunc func(ctx context.Context, tx repository.TxStore, userID int64, c *domain.RedemptionCode) error

// appliers 类型 → applier 注册表（决策 1）：新类型 = 注册新 applier，兑换编排零改动。
var appliers = map[domain.RedemptionType]applyFunc{
	domain.RedemptionTypeBalance:     applyBalance,
	domain.RedemptionTypeConcurrency: applyConcurrency,
	domain.RedemptionTypeTempBalance: applyTempBalance,
}

// applyBalance 余额累加：users.balance += value（原子 SQL，无读改写——评审 I-1）。
func applyBalance(ctx context.Context, tx repository.TxStore, userID int64, c *domain.RedemptionCode) error {
	return tx.UpdateUserBalance(ctx, userID, c.Value)
}

// applyConcurrency 并发上限（决策 2）：0 = 不限特判——当前 0 直接设为 value，
// 非 0 累加（0 语义在 SQL CASE 内，单语句无读改写——评审 I-1）。
func applyConcurrency(ctx context.Context, tx repository.TxStore, userID int64, c *domain.RedemptionCode) error {
	return tx.UpdateUserMaxConcurrency(ctx, userID, int(c.Value))
}

// applyTempBalance 临时额度（决策 4）：插入 TempBalance 行（amount=value，
// expires_at=resource_expires_at，note="redemption code"）。
// resource_expires_at 缺失 = 数据异常，按无效码 400 处理。
func applyTempBalance(ctx context.Context, tx repository.TxStore, userID int64, c *domain.RedemptionCode) error {
	if c.ResourceExpiresAt == nil {
		return fmt.Errorf("%w: invalid code", ErrInvalidInput)
	}
	note := "redemption code"
	return tx.CreateTempBalance(ctx, userID, c.Value, c.ResourceExpiresAt, &note)
}

// Redeem 兑换（/api/user/redemptions POST，决策 7/10-12 编排）：
// 单事务内按序——① GetByCode 定位码（不存在 → 400 invalid code）；
// ② GetUse 先查本用户已兑换（评审 M-1：重复请求稳定 409，不因码状态漂移）；
// ③ 码状态检查（disabled/过期 → 400 invalid code，统一不泄露具体原因）；
// ④ applier 应用资源（只经 tx 面，失败整体回滚——评审 I-1）；
// ⑤ CreateUse 审计 + IncrementUsed 条件递增（false = 用尽 → 400 整体回滚，
// 防并发超卖——评审 I-2）。提交成功后 invalidate() 刷新 auth 快照（决策 8）。
func (s *Service) Redeem(ctx context.Context, code string, userID int64) (*domain.RedemptionApply, error) {
	var apply *domain.RedemptionApply
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		c, err := tx.GetByCode(ctx, code)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return fmt.Errorf("%w: invalid code", ErrInvalidInput)
			}
			return err
		}
		// 先查 use（评审 M-1）：本用户已兑换 → 409（码随后失效也仍回 409，
		// 不与"已兑换"事实矛盾）
		if _, err := tx.GetUse(ctx, c.ID, userID); err == nil {
			return fmt.Errorf("%w: already redeemed", ErrConflict)
		} else if !errors.Is(err, repository.ErrNotFound) {
			return err
		}
		// 码状态：disabled/过期 → 400 invalid code（统一，不泄露状态细节）
		if c.Status != domain.RedemptionStatusActive ||
			(c.ExpiresAt != nil && !c.ExpiresAt.After(time.Now())) {
			return fmt.Errorf("%w: invalid code", ErrInvalidInput)
		}
		// applier 注册表（决策 1）：新类型 = 注册新 applier，编排零改动
		fn, ok := appliers[c.Type]
		if !ok {
			return fmt.Errorf("%w: invalid code", ErrInvalidInput)
		}
		if err := fn(ctx, tx, userID, c); err != nil {
			return mapRepoErr(err)
		}
		if err := tx.CreateUse(ctx, &domain.RedemptionUse{
			CodeID: c.ID, UserID: userID, Value: c.Value,
			ResourceExpiresAt: c.ResourceExpiresAt,
		}); err != nil {
			return mapRepoErr(err) // (code_id,user_id) 唯一冲突 → 409（DB 兜底幂等）
		}
		ok, err = tx.IncrementUsed(ctx, c.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: invalid code", ErrInvalidInput) // 用尽（评审 I-2）
		}
		apply = &domain.RedemptionApply{Type: c.Type, Value: c.Value, ResourceExpiresAt: c.ResourceExpiresAt}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.inv.Users() // 余额/并发已变更 → Auth + 余额快照刷新（O2 矩阵：用户面变更，去抖窗口内全量）
	s.publish(ctx, notify.Change{Users: true})
	return apply, nil
}
