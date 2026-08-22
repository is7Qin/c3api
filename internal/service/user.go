// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/pkg/logx"
)

// 错误哨兵定义下沉 internal/service/errors（叶子包，单一真相）；别名
// re-export 保持既有引用同一实例语义。
var (
	ErrInvalidCredentials = serviceerr.ErrInvalidCredentials
	ErrSignupDisabled     = serviceerr.ErrSignupDisabled
)

// RegisterUser 注册（注册即登录——handler 侧签发 JWT）：
// 快照读 settings.signup_enabled 开关（UpdateSetting 即时生效）→ email
// 唯一/格式 → 密码 ≤72 字节 → bcrypt DefaultCost(10)（sub2api 同参数）→
// 快照读 4 个新用户初始资源默认值 → CreateUser → temp_balance > 0 送临时
// 额度（插行失败不阻断注册，评审 M-2）。
func (s *Service) RegisterUser(ctx context.Context, email, password string) (*domain.User, error) {
	if s.settingValue("signup_enabled") != "true" {
		return nil, ErrSignupDisabled
	}
	if !validEmail(email) {
		return nil, ErrInvalidInput
	}
	if password == "" || auth.ValidatePasswordLen(password) != nil {
		return nil, ErrInvalidInput
	}
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrConflict // email 唯一 → 409
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	// 首个注册用户 bootstrap（方案 A，spec 2026-08-15）：users 表空时第一个
	// 注册用户 = platform_admin（管理面无需静态 token 即可登录）；一旦有人
	// 注册（n > 0），后续注册恒为普通 user——无需额外机制。
	// 竞态：表空并发双注册在 READ COMMITTED 下两个 count 均见 0 → 双 admin；
	// 不引入锁——增量后果 = 抢注窗口从"严格先到"放宽为"同刻并发"（bootstrap
	// 秒级窗口 + 毫秒竞速才触发，多出的非预期 admin 账号可删）。
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return nil, err
	}
	role := domain.RoleUser
	if n == 0 {
		role = domain.RolePlatformAdmin
	}
	// 新用户初始资源：仅公开注册路径套默认；管理面 CreateUser 显式传值
	// （用户拍板，0 就是 0）。
	created, err := s.store.CreateUser(ctx, &domain.User{
		Email: email, PasswordHash: hash,
		Role: role, Status: domain.UserStatusActive,
		MaxConcurrency: int(s.settingInt("default_user_max_concurrency")),
		Balance:        s.settingInt("default_user_balance"),
	})
	if err != nil {
		// 并发重复邮箱：双过 pre-check 后一者撞 DB 唯一冲突 → repo 已映射
		// ErrConflict → 这里映射 409（不映射 → 裸错 500）。
		return nil, mapRepoErr(err)
	}
	if temp := s.settingInt("default_user_temp_balance"); temp > 0 {
		expiresAt := time.Now().AddDate(0, 0, int(s.settingInt("default_user_temp_balance_ttl_days")))
		note := "signup bonus"
		if err := s.store.CreateTempBalance(ctx, created.ID, temp, &expiresAt, &note); err != nil {
			// 评审 M-2：赠品插行失败不阻断注册（否则注册报错 → 客户端重试
			// → 409 email 死锁）；仅告警，用户已创建成功。
			if s.log != nil {
				s.log.Warn("signup temp balance insert failed", logx.Int64("user_id", created.ID), logx.Error(err))
			}
		}
	}
	s.upsertUserSnapshot(created)
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	if s.log != nil {
		s.log.Info("user registered", logx.Int64("id", created.ID), logx.String("email", email))
	}
	return created, nil
}

// LoginUser 登录校验：email → bcrypt 校验 → 状态检查（禁用与口令错误同
// 文案 401，防枚举）。
func (s *Service) LoginUser(ctx context.Context, email, password string) (*domain.User, error) {
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil || !auth.VerifyPassword(u.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	if u.Status != domain.UserStatusActive {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// ListUserTempBalances 当前用户有效临时额度（/api/user/temp-balances；userID 由
// handler 强制 = 当前用户，无 user_id 参数防越权——对齐 /api/user/stats 模式）。
func (s *Service) ListUserTempBalances(ctx context.Context, userID int64) ([]*domain.TempBalance, error) {
	return s.store.ListUserTempBalances(ctx, userID)
}

// ListTempBalances 管理侧临时额度全量列表（/api/admin/temp-balances；userID 0 =
// 全部用户；sort/order 白名单校验——非法 → ErrInvalidInput 400）。
func (s *Service) ListTempBalances(ctx context.Context, q repository.ListQuery, userID int64) ([]*domain.TempBalance, int64, error) {
	if err := validateListQuery(q, listSortFields["temp_balances"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListTempBalances(ctx, q, userID)
}

// ChangePassword 修改密码（/api/user/auth/change-password）：旧密码校验复用登录
// 语义（bcrypt 校验 + 状态检查——失败 ErrInvalidCredentials 401 同登录文案
// 防枚举）；新密码非空 + ≤72 字节（bcrypt 截断限制，注册/建用户同款校验）→
// 非法 ErrInvalidInput 400；成功 bcrypt 重哈希落库。**不撤销既有 JWT**——
// 无状态 token 无撤销机制，新密码下次登录生效（注释契约）。
func (s *Service) ChangePassword(ctx context.Context, userID int64, old, new string) error {
	// 新密码校验前置（廉价无 DB；非法 400 不触达旧密码判定）。
	if new == "" || auth.ValidatePasswordLen(new) != nil {
		return ErrInvalidInput
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return mapRepoErr(err)
	}
	if !auth.VerifyPassword(u.PasswordHash, old) || u.Status != domain.UserStatusActive {
		return ErrInvalidCredentials
	}
	hash, err := auth.HashPassword(new)
	if err != nil {
		return err
	}
	return s.store.UpdateUserPassword(ctx, userID, hash)
}

// GetUser 用户详情（/api/admin/users/{id} 更新前置读取）。
func (s *Service) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	u, err := s.store.GetUser(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return u, nil
}

// GetUserMe 当前用户信息（/api/user/auth/me）。
func (s *Service) GetUserMe(ctx context.Context, userID int64) (*domain.User, error) {
	return s.GetUser(ctx, userID)
}

// CreateUser 管理面创建用户（platform_admin 专属）：email 唯一/格式、密码
// ≤72 字节 → bcrypt（sub2api 同参数）→ role/status/max_concurrency/balance
// 落库 → invalidate（新用户入 Auth 状态快照）。价格倍率按组（T3.5 修正）经
// group_assignment 设置（SetGroupAssignments），用户本体无倍率字段。
func (s *Service) CreateUser(ctx context.Context, email, password string, role domain.Role, status domain.UserStatus, maxConcurrency int, balance int64) (*domain.User, error) {
	if !validEmail(email) {
		return nil, ErrInvalidInput
	}
	if password == "" || auth.ValidatePasswordLen(password) != nil {
		return nil, ErrInvalidInput
	}
	if !role.Valid() || !status.Valid() || maxConcurrency < 0 || balance < 0 {
		return nil, ErrInvalidInput
	}
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrConflict // email 唯一 → 409
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	created, err := s.store.CreateUser(ctx, &domain.User{
		Email: email, PasswordHash: hash,
		Role: role, Status: status,
		MaxConcurrency: maxConcurrency, Balance: balance,
	})
	if err != nil {
		return nil, err
	}
	s.upsertUserSnapshot(created)
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	if s.log != nil {
		s.log.Info("user created by admin", logx.Int64("id", created.ID), logx.String("email", email), logx.String("role", string(role)))
	}
	return created, nil
}

// ListUsers 用户列表（/api/admin/users；platform_admin 专属）。
func (s *Service) ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error) {
	if err := validateListQuery(q, listSortFields["users"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListUsers(ctx, q)
}

// maxUserUpdateRetries 条件更新 0 行（期间有扣费/并发变更）→ 重读重试上限
// （v02 修复方向：重试时 new 保持管理员显式意图，仅刷新旧值条件）。
const maxUserUpdateRetries = 3

// UpdateUser 用户管理面更新（role/status/max_concurrency/balance 按 patch 显式
// 字段生效——未提供字段不触碰 DB 列，杜绝 GET 快照陈旧值写回覆盖计费扣费）。
// 校验按 patch 字段生效（评审 P3-B：只改 balance 的 PUT 不被未提供字段的
// 零值误拒）。balance/max_concurrency 显式设置 → 条件更新（旧值 = GET 快照）；
// 0 行 → 重读当前值刷新旧值条件重试 ≤3 次 → 超限 ErrConflict（409）。用户
// 状态/并发/额度变更 → invalidate → Auth.Reload 全量刷新（评审 I-2）。价格
// 倍率按组（T3.5 修正）经 group_assignment 设置，用户本体无倍率字段。
func (s *Service) UpdateUser(ctx context.Context, p *repository.UserPatch) (*domain.User, error) {
	if p.Role != nil && !p.Role.Valid() {
		return nil, ErrInvalidInput
	}
	if p.Status != nil && !p.Status.Valid() {
		return nil, ErrInvalidInput
	}
	if p.MaxConcurrency != nil && *p.MaxConcurrency < 0 {
		return nil, ErrInvalidInput
	}
	if p.Balance != nil && *p.Balance < 0 {
		return nil, ErrInvalidInput
	}
	for attempt := 0; ; attempt++ {
		updated, err := s.store.UpdateUser(ctx, p)
		if err == nil {
			s.upsertUserSnapshot(updated)
			s.inv.Users()
			s.publish(ctx, notify.Change{Users: true})
			return updated, nil
		}
		if !errors.Is(err, repository.ErrConflict) || attempt >= maxUserUpdateRetries {
			return nil, mapRepoErr(err)
		}
		// 条件不满足（期间有扣费）：重读当前值刷新旧值条件，new 不变重试。
		cur, err := s.store.GetUser(ctx, p.ID)
		if err != nil {
			return nil, mapRepoErr(err)
		}
		p.OldMaxConcurrency = &cur.MaxConcurrency
		p.OldBalance = &cur.Balance
	}
}

// userSnapshotUpdater Auth 快照用户表的本地增量写面（proxy.Auth.UpsertUser）。
// 仅本地立即可见，跨实例仍经 NOTIFY 全量 Reload；不存在则 no-op（测试 fake 不实现）。
type userSnapshotUpdater interface {
	UpsertUser(userID int64, snap domain.UserSnapshot)
}

// upsertUserSnapshot 本地立即可见的用户快照写入（不等 200ms 去抖窗口）。
// 新用户 401 窗口的根因修复：创建后 RequireJWT 立即可查 active 状态。
func (s *Service) upsertUserSnapshot(u *domain.User) {
	if s.keys == nil || u == nil {
		return
	}
	if upd, ok := s.keys.(userSnapshotUpdater); ok {
		upd.UpsertUser(u.ID, domain.UserSnapshot{Status: u.Status, Role: u.Role})
	}
}

// validEmail 简单邮箱格式校验（net/mail 解析 + 纯地址形式）。
func validEmail(email string) bool {
	if len(email) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return false
	}
	if strings.Contains(email, "..") {
		return false
	}
	return true
}
