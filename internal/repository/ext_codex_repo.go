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
	"github.com/is7qin/c3api/internal/ent/accountext"
)

// AccountExtRepo 账号类型化鉴权扩展（account_ext 1:1 边缘表；codex 专用——
// credential_type ∈ {codex-oauth, codex-pat}，codex 列组：codex_identity
// （身份四元组 jsonb，service 导入时自动生成、持久复用）+ codex_oauth_* 组
// + codex_pat_key 组；列组类型约束由 service 校验）。未来 claude oauth 等
// 新类型 → 新增 ext_claude_repo.go 同构。
type AccountExtRepo struct{ client *ent.Client }

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

// WriteOAuthRotation 轮转回写（SDK 接入 T5 §1——SDK OnTokenRotated 回调落库
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
// SDK D4 回调重试 → 连续达阈值 CallbackDeliveryError fatal（fail-closed：
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

// FindAccountExtByCodexKey 组合幂等键查重（Task B 批量导入专用——
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

// WritePATKey pat 凭据部分更新（Task B 批量导入 updated 路径；WriteOAuthRotation
// 的 pat 对称形态——只 SetCodexPatKey，identity/email/oauth 列零触碰；全量
// UpsertAccountExt 的 ClearX 清 NULL 面在此禁用）。行缺失 → ErrNotFound（同
// WriteOAuthRotation 语义——updated 路径行必存在，dedup 刚查到，不触发）。
func (r *AccountExtRepo) WritePATKey(ctx context.Context, accountID int64, patKey string) error {
	n, err := r.client.AccountExt.Update().
		Where(accountext.AccountIDEQ(accountID)).
		SetCodexPatKey(patKey).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: account_id=%d ext row missing (codex account must have account_ext)", ErrNotFound, accountID)
	}
	return nil
}

// GetAccountExt 按账号 id 取扩展行；无行 → ErrNotFound。
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
