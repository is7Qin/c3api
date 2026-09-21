// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/accountext"
)

// AccountExtRepo 账号类型化鉴权扩展（account_ext 1:1 边缘表；codex 专用——
// credential_type ∈ {codex-oauth, codex-pat}，codex 列组：codex_identity
// （身份四元组 jsonb，service 导入时自动生成、持久复用）+ codex_oauth_* 组
// + codex_pat_key 组；列组类型约束由 service 校验）。未来 claude oauth 等
// 新类型 → 新增 ext_claude_repo.go 同构。
type AccountExtRepo struct{ client *ent.Client }

// extIdentityField 标识 ext 凭据面里参与"是否改身份"判定的字段。凭据面整体是
// 身份类（spec §5.5 的 ext 行）：管理面换凭据即换账号身份 ⇒ 推进 K ⇒ 在途判定
// （失效判决 / latch / 健康记录 / continuation）作废。
//
// 凭据值（token / refresh / expires）也在列，因为管理面写入是**凭据替换**：
// 身份四元组与 digest 之外的凭据材料同样只由这次写入决定。SDK 自动刷新走
// WriteOAuthRotation（不触 accounts 行）——它天然 K 中性，"刷新同一账号的令牌"
// 不是身份写入。
type extIdentityField uint8

const (
	extCredentialType extIdentityField = iota
	extIdentityQuad
	extOAuthToken
	extOAuthRefresh
	extOAuthExpires
	extPATKey
	extEmail
	extCodexAccountID
)

// extIdentityChanged 报告 next 相对既有行 cur 在**指定字段**上是否按值变更。
// 判定统一为"归一后按值"：指针 nil 与空值等价（该写面的 nil = 清空）。
// cur == nil（无既有行 = 首次创建）视为变更。
//
// 这是 ext 侧唯一的身份变更判据：三个管理面凭据写点都调用它，不得各自重写
// 一份字段清单。幂等重写（值未变）返回 false ⇒ 只推进 C、不推进 K（spec §3.5）。
func extIdentityChanged(cur, next *domain.AccountExt, fields ...extIdentityField) bool {
	if cur == nil {
		return true
	}
	if next == nil {
		return false
	}
	for _, f := range fields {
		switch f {
		case extCredentialType:
			if next.CredentialType != cur.CredentialType {
				return true
			}
		case extIdentityQuad:
			if !sameCodexIdentity(next.CodexIdentity, cur.CodexIdentity) {
				return true
			}
		case extOAuthToken:
			if derefString(next.CodexOAuthToken) != derefString(cur.CodexOAuthToken) {
				return true
			}
		case extOAuthRefresh:
			if derefString(next.CodexOAuthRefreshToken) != derefString(cur.CodexOAuthRefreshToken) {
				return true
			}
		case extOAuthExpires:
			if !sameTimePtr(next.CodexOAuthExpiresAt, cur.CodexOAuthExpiresAt) {
				return true
			}
		case extPATKey:
			if derefString(next.CodexPATKey) != derefString(cur.CodexPATKey) {
				return true
			}
		case extEmail:
			if derefString(next.CodexEmail) != derefString(cur.CodexEmail) {
				return true
			}
		case extCodexAccountID:
			if derefString(next.CodexAccountID) != derefString(cur.CodexAccountID) {
				return true
			}
		}
	}
	return false
}

// sameCodexIdentity 身份四元组按值相等（nil 与 nil 相等）。
func sameCodexIdentity(a, b *domain.CodexIdentity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// derefString nil 安全的字符串取值：凭据面比较里 nil 与空串同值（该写面 nil = 清空）。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sameTimePtr 时刻指针按值相等（nil 与 nil 相等）。
func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// loadAccountExtInTx 读事务内的 ext 行；无行 → (nil, nil)（首次创建是"变更"）。
func loadAccountExtInTx(ctx context.Context, tx *ent.Client, accountID int64) (*domain.AccountExt, error) {
	row, err := tx.AccountExt.Query().Where(accountext.AccountIDEQ(accountID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}

// bumpAccountGeneration 在同一个 UPDATE 里推进 C（无条件）并按需推进 K：C 用
// **相对自增**（与 guard 解耦——把 expected 写进 C 会同时破坏客户端 CAS 令牌与
// 重编译水位），guard 仍是 C 前置条件。
func bumpAccountGeneration(ctx context.Context, tx *ent.Client, accountID, expectedRevision int64, advanceIdentity bool) (int, error) {
	u := tx.Account.Update().
		Where(account.IDEQ(accountID), account.LifecycleRevisionEQ(expectedRevision)).
		AddLifecycleRevision(1)
	if advanceIdentity {
		u = u.AddIdentityRevision(1)
	}
	return u.Save(ctx)
}

// UpsertAccountExt 幂等写入（同 TemplateExtRepo.UpsertTemplateExt 语义：
// 冲突 UPDATE 显式 ClearX 清 NULL）。codex_identity 必存（nil 由 service
// 生成/沿用——正常路径恒非 nil；jsonb NULL → ClearX 清空，身份无清空路径）；
// codex_oauth_* / codex_pat_key / codex_email 由 service 维护（nil → 清空）。
func (r *AccountExtRepo) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	_, err := r.client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetCodexIdentity(e.CodexIdentity).
		SetNillableCodexOauthToken(e.CodexOAuthToken).
		SetNillableCodexOauthRefreshToken(e.CodexOAuthRefreshToken).
		SetNillableCodexOauthExpiresAt(e.CodexOAuthExpiresAt).
		SetNillableCodexPatKey(e.CodexPATKey).
		SetNillableCodexEmail(e.CodexEmail).
		SetNillableCodexAccountID(e.CodexAccountID).
		OnConflictColumns(accountext.FieldAccountID).
		Update(func(u *ent.AccountExtUpsert) {
			u.SetCredentialType(string(e.CredentialType))
			if e.CodexIdentity != nil {
				u.SetCodexIdentity(e.CodexIdentity)
			} else {
				u.ClearCodexIdentity()
			}
			if e.CodexOAuthToken != nil {
				u.SetCodexOauthToken(*e.CodexOAuthToken)
			} else {
				u.ClearCodexOauthToken()
			}
			if e.CodexOAuthRefreshToken != nil {
				u.SetCodexOauthRefreshToken(*e.CodexOAuthRefreshToken)
			} else {
				u.ClearCodexOauthRefreshToken()
			}
			if e.CodexOAuthExpiresAt != nil {
				u.SetCodexOauthExpiresAt(*e.CodexOAuthExpiresAt)
			} else {
				u.ClearCodexOauthExpiresAt()
			}
			if e.CodexPATKey != nil {
				u.SetCodexPatKey(*e.CodexPATKey)
			} else {
				u.ClearCodexPatKey()
			}
			if e.CodexEmail != nil {
				u.SetCodexEmail(*e.CodexEmail)
			} else {
				u.ClearCodexEmail()
			}
			if e.CodexAccountID != nil {
				u.SetCodexAccountID(*e.CodexAccountID)
			} else {
				u.ClearCodexAccountID()
			}
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetAccountExt(ctx, e.AccountID)
}

// TryInsertAccountExt 先写者胜的空插入（首写原子性——并发双导入同一账号时
// 保持先写身份：ON CONFLICT (account_id) DO NOTHING，冲突行跳过不覆盖不报错）。
// 返回是否实际插入：true = 本请求首写胜出（行已含本次全量字段）；
// false = 冲突（已有行），调用方应沿用存量身份后走 UpsertAccountExt 写令牌。
// 账号缺 id（FK）→ error。
func (r *AccountExtRepo) TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error) {
	err := r.client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetCodexIdentity(e.CodexIdentity).
		SetNillableCodexOauthToken(e.CodexOAuthToken).
		SetNillableCodexOauthRefreshToken(e.CodexOAuthRefreshToken).
		SetNillableCodexOauthExpiresAt(e.CodexOAuthExpiresAt).
		SetNillableCodexPatKey(e.CodexPATKey).
		SetNillableCodexEmail(e.CodexEmail).
		SetNillableCodexAccountID(e.CodexAccountID).
		OnConflictColumns(accountext.FieldAccountID).
		DoNothing().
		Exec(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // 冲突：先写者已落行，本次跳过（不覆盖）
	}
	return false, err
}

// WriteOAuthRotation 轮转回写（SDK 接入 §1——SDK OnTokenRotated 回调落库
// 面）：account_ext **部分更新**（幂等收敛语义与 upsert 等价——重复回调重复
// UPDATE 收敛）仅 codex_oauth_token / codex_oauth_refresh_token /
// codex_oauth_expires_at 三列——其余列不动（避免 UpsertAccountExt 全量
// upsert 的 ClearX 清空：nil 字段会把 codex_identity/pat/email 等列清 NULL，
// ext_codex_repo.go:43-85）。
//
// expiresAt 为调用方携带的旧过期时刻（SDK 回调无 expiry、refreshResponse 无
// expires_in——保旧语义：恒写携带值，不引入新值；nil = 写 NULL，与"未知过
// 期"语义一致）。at/rt 由 SDK 保证非空（响应缺 refresh_token 时 SDK 保留内
// 存旧 rt 后回调——盲写不落空）。
//
// 行缺失（配置损坏——codex 账号必有 ext 行，选号前提）→ 0 行报错：错误 →
// SDK 回调重试 → 连续达阈值 CallbackDeliveryError fatal（fail-closed：
// 令牌无法持久化 = 账号失效信号，管理员重新导入后恢复）。不做 INSERT 路径
// ——回调无身份/类型材料（缺行场景 = 配置损坏，应报错而非以空身份静默建行）。
func (r *AccountExtRepo) WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error {
	u := r.client.AccountExt.Update().
		Where(accountext.AccountIDEQ(accountID)).
		SetCodexOauthToken(at).
		SetCodexOauthRefreshToken(rt)
	if expiresAt != nil {
		u = u.SetCodexOauthExpiresAt(*expiresAt)
	} else {
		u = u.ClearCodexOauthExpiresAt() // SetNillable(nil) 是 no-op——保旧 nil 需显式清
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: account_id=%d ext row missing (codex account must have account_ext)", ErrNotFound, accountID)
	}
	return nil
}

// FindAccountExtByCodexKey 组合幂等键查重（批量导入专用——
// (codex_email, codex_account_id) 双条件 AND 定位，对齐唯一索引；GetAccountExt
// 仅按 account_id，查重面不存在）。命中返回行（含 credential_type——跨类型
// 判定用）；缺行 → ErrNotFound。
func (r *AccountExtRepo) FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string) (*domain.AccountExt, error) {
	row, err := r.client.AccountExt.Query().
		Where(accountext.CodexEmailEQ(codexEmail), accountext.CodexAccountIDEQ(codexAccountID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: codex_email=%q codex_account_id=%q missing", ErrNotFound, codexEmail, codexAccountID)
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}

// GetAccountExt 按账号取类型化鉴权扩展；缺行 → ErrNotFound。
func (r *AccountExtRepo) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	row, err := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(accountID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: account_id=%d missing", ErrNotFound, accountID)
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}

// AdminWriteOAuthRotationCAS 管理员 OAuth 轮转（fenced）：CAS expectedRevision，
// 无条件推进 C，并在凭据面**按值变更**时推进 K；ext 三列与计数器同事务。
// SDK 内部刷新继续使用 WriteOAuthRotation（unfenced，不触 accounts 行、K 中性）。
func (r *AccountExtRepo) AdminWriteOAuthRotationCAS(ctx context.Context, accountID int64, expectedRevision int64, at, rt string, expiresAt *time.Time) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	cur, err := loadAccountExtInTx(ctx, tx.Client(), accountID)
	if err != nil {
		return err
	}
	advanceIdentity := extIdentityChanged(cur, &domain.AccountExt{
		CodexOAuthToken:        &at,
		CodexOAuthRefreshToken: &rt,
		CodexOAuthExpiresAt:    expiresAt,
	}, extOAuthToken, extOAuthRefresh, extOAuthExpires)
	n, err := bumpAccountGeneration(ctx, tx.Client(), accountID, expectedRevision, advanceIdentity)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: account_id=%d expected revision %d stale", ErrStaleRevision, accountID, expectedRevision)
	}
	u := tx.AccountExt.Update().Where(accountext.AccountIDEQ(accountID)).SetCodexOauthToken(at).SetCodexOauthRefreshToken(rt)
	if expiresAt != nil {
		u = u.SetCodexOauthExpiresAt(*expiresAt)
	} else {
		u = u.ClearCodexOauthExpiresAt()
	}
	n2, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n2 == 0 {
		return fmt.Errorf("%w: account_id=%d ext row missing", ErrNotFound, accountID)
	}
	return tx.Commit()
}

// AdminWritePATKeyCAS 管理员 PAT 轮转（fenced）：同 AdminWriteOAuthRotationCAS。
func (r *AccountExtRepo) AdminWritePATKeyCAS(ctx context.Context, accountID int64, expectedRevision int64, patKey string) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := loadAccountExtInTx(ctx, tx.Client(), accountID)
	if err != nil {
		return err
	}
	advanceIdentity := extIdentityChanged(cur, &domain.AccountExt{CodexPATKey: &patKey}, extPATKey)
	n, err := bumpAccountGeneration(ctx, tx.Client(), accountID, expectedRevision, advanceIdentity)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: account_id=%d expected revision %d stale", ErrStaleRevision, accountID, expectedRevision)
	}
	n2, err := tx.AccountExt.Update().Where(accountext.AccountIDEQ(accountID)).SetCodexPatKey(patKey).Save(ctx)
	if err != nil {
		return err
	}
	if n2 == 0 {
		return fmt.Errorf("%w: account_id=%d ext row missing", ErrNotFound, accountID)
	}
	return tx.Commit()
}

// AdminUpsertAccountExtCAS 管理员 PUT /ext（fenced）：CAS revision，无条件推进 C，
// 并在凭据面**按值变更**时推进 K；插入/更新 ext 与计数器同事务。幂等 PUT（值未变）
// 只推进 C（spec §3.5：不做"端点调用即推进"）。
func (r *AccountExtRepo) AdminUpsertAccountExtCAS(ctx context.Context, e *domain.AccountExt, expectedRevision int64) (*domain.AccountExt, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := loadAccountExtInTx(ctx, tx.Client(), e.AccountID)
	if err != nil {
		return nil, err
	}
	advanceIdentity := extIdentityChanged(cur, e,
		extCredentialType, extIdentityQuad, extOAuthToken, extOAuthRefresh,
		extOAuthExpires, extPATKey, extEmail, extCodexAccountID)
	n, err := bumpAccountGeneration(ctx, tx.Client(), e.AccountID, expectedRevision, advanceIdentity)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: account_id=%d expected revision %d stale", ErrStaleRevision, e.AccountID, expectedRevision)
	}
	// Upsert ext via tx
	_, err = tx.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetCodexIdentity(e.CodexIdentity).
		SetNillableCodexOauthToken(e.CodexOAuthToken).
		SetNillableCodexOauthRefreshToken(e.CodexOAuthRefreshToken).
		SetNillableCodexOauthExpiresAt(e.CodexOAuthExpiresAt).
		SetNillableCodexPatKey(e.CodexPATKey).
		SetNillableCodexEmail(e.CodexEmail).
		SetNillableCodexAccountID(e.CodexAccountID).
		OnConflictColumns(accountext.FieldAccountID).
		Update(func(u *ent.AccountExtUpsert) {
			u.SetCredentialType(string(e.CredentialType))
			if e.CodexIdentity != nil {
				u.SetCodexIdentity(e.CodexIdentity)
			} else {
				u.ClearCodexIdentity()
			}
			if e.CodexOAuthToken != nil {
				u.SetCodexOauthToken(*e.CodexOAuthToken)
			} else {
				u.ClearCodexOauthToken()
			}
			if e.CodexOAuthRefreshToken != nil {
				u.SetCodexOauthRefreshToken(*e.CodexOAuthRefreshToken)
			} else {
				u.ClearCodexOauthRefreshToken()
			}
			if e.CodexOAuthExpiresAt != nil {
				u.SetCodexOauthExpiresAt(*e.CodexOAuthExpiresAt)
			} else {
				u.ClearCodexOauthExpiresAt()
			}
			if e.CodexPATKey != nil {
				u.SetCodexPatKey(*e.CodexPATKey)
			} else {
				u.ClearCodexPatKey()
			}
			if e.CodexEmail != nil {
				u.SetCodexEmail(*e.CodexEmail)
			} else {
				u.ClearCodexEmail()
			}
			if e.CodexAccountID != nil {
				u.SetCodexAccountID(*e.CodexAccountID)
			} else {
				u.ClearCodexAccountID()
			}
		}).ID(ctx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.GetAccountExt(ctx, e.AccountID)
}
