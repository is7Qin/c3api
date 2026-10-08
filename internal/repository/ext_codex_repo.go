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

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/accountext"
)

// AccountExtRepo 账号类型化鉴权扩展（account_ext 1:1 边缘表；codex 专用——
// credential_type ∈ {codex-oauth, codex-pat}，codex 列组：codex_identity
// （持久身份 jsonb，仅 installation_id；service 导入时自动生成、持久复用）+
// codex_oauth_* 组 + codex_pat_key 组；列组类型约束由 service 校验）。未来
// claude oauth 等新类型 → 新增 ext_claude_repo.go 同构。
type AccountExtRepo struct {
	client *ent.Client
	// driver 为原始 dialect.Driver：TryInsert 的 owner 锁账号（SELECT ... FOR UPDATE）
	// 走 raw SQL（ent 生成器无 FOR UPDATE 入口）；WithTx/tx 面事务驱动保证 raw SQL
	// 与 ent 构建器同连接。
	driver dialect.Driver
}

// extIdentityField 标识 ext 凭据面里参与"是否改身份"判定的字段。凭据面整体是
// 身份类（spec §5.5 的 ext 行）：管理面换凭据即换账号身份 ⇒ 推进 K ⇒ 在途判定
// （失效判决 / latch / 健康记录 / continuation）作废。
//
// 凭据值（token / refresh / expires）也在列，因为管理面写入是**凭据替换**：
// 持久身份与 digest 之外的凭据材料同样只由这次写入决定。SDK 自动刷新走
// WriteOAuthRotation（不触 accounts 行）——它天然 K 中性，"刷新同一账号的令牌"
// 不是身份写入。
type extIdentityField uint8

const (
	extCredentialType extIdentityField = iota
	extIdentity
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
		case extIdentity:
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

// sameCodexIdentity 持久身份按值相等（nil 与 nil 相等；现只含 installation_id）。
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
//
// **作用域（§2.5）**：ctx 带作用域时 owner 谓词同样 AND 进本 UPDATE——越域账号
// 影响 0 行，调用方按「stale/越域」同一分支拒绝（不写他人行、不推进他人代际）。
func bumpAccountGeneration(ctx context.Context, tx *ent.Client, accountID, expectedRevision int64, advanceIdentity bool) (int, error) {
	u := tx.Account.Update().
		Where(account.IDEQ(accountID), account.LifecycleRevisionEQ(expectedRevision)).
		AddLifecycleRevision(1)
	u = accountOwnerUpdate(ctx, u)
	if advanceIdentity {
		u = u.AddIdentityRevision(1)
	}
	return u.Save(ctx)
}

// requireOwnedAccountInTx 在**写事务内**以 owner 谓词复核账号存在且落在当前作用
// 域内（C1，spec §2.5）：越域/缺失 ⇒ ErrNotFound，且**早于任何 ext 写入**——账号
// 存在性、CAS 与 ext 变更因此落在同一事务边界与同一 owner 谓词内（不先取行再应用
// 层判权，也不留两次查询之间的转属窗口）。作用域未注入（管理面）时谓词为空 ⇒
// 仅校验账号存在。
func requireOwnedAccountInTx(ctx context.Context, client *ent.Client, accountID int64) error {
	ok, err := client.Account.Query().
		Where(account.IDEQ(accountID)).
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
		Exist(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: account_id=%d missing or outside scope", ErrNotFound, accountID)
	}
	return nil
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
// 账号缺 id（FK）/ 越域 → error（越域 ⇒ ErrNotFound）。
//
// **作用域（§2.5）**：插入与「账号归属可见性」在同一事务与同一 owner 锁边界内完成
// ——写前以 owner 谓词 `SELECT ... FOR UPDATE` **锁账号行**（越域/缺失 ⇒ ErrNotFound，
// 不落 ext 行），再在同一事务内 INSERT。并发转属必须更新同一 accounts 行，故被本锁
// 挡住、直到本事务结束——不存在「先无锁 Exist 通过、另一连接转属并提交、随后 INSERT
// 不复核 owner」的窗口（spec S:139/S:826）。
func (r *AccountExtRepo) TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error) {
	var inserted bool
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, txDrv dialect.Driver) error {
		if err := lockOwnedAccountForUpdate(ctx, txDrv, e.AccountID); err != nil {
			return err
		}
		err := client.AccountExt.Create().
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
		inserted = err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil // err == sql.ErrNoRows：冲突，先写者已落行（不覆盖）
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// lockOwnedAccountForUpdate 在**写事务内**以 owner 谓词加 `FOR UPDATE` 锁账号行
// （spec §2.5）：越域/缺失 ⇒ ErrNotFound（行数 0）。作用域未注入（管理面）时仅锁
// 账号存在。持锁期间并发转属（UPDATE accounts ... supplier_user_id）会阻塞在行锁
// 上，直到本事务结束——故「owner 复核」与「ext 写/读」之间无转属窗口。
func lockOwnedAccountForUpdate(ctx context.Context, driver dialect.Driver, accountID int64) error {
	query := `SELECT id FROM accounts WHERE id = $1`
	args := []any{accountID}
	if s := domain.AccountScopeFrom(ctx); s.Set {
		args = append(args, s.OwnerUID)
		query += fmt.Sprintf(" AND supplier_user_id = $%d", len(args))
	}
	query += " FOR UPDATE"
	rows := &entsql.Rows{}
	if err := driver.Query(ctx, query, args, rows); err != nil {
		return err
	}
	defer rows.Close() // nolint:errcheck // Rows.Err reports iteration failures.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: account_id=%d missing or outside scope", ErrNotFound, accountID)
	}
	return nil
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
//
// **作用域（§2.5）**：供应商面（ctx 带作用域）时 owner 谓词经子查询 AND 进**同
// 一条 SQL**——他人归属的同键行查不到，于是不会走「先全局命中、再应用层比归属」
// 的 TOCTOU 路径（那正是 spec 明令禁止的形状）。跨归属同键随后撞全局唯一索引
// （23505）⇒ 行级 failed，不改他人行（调用方把它翻成不透出他人 id 的文案）。
func (r *AccountExtRepo) FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string) (*domain.AccountExt, error) {
	row, err := r.accountExtQuery(ctx).
		Where(accountext.CodexEmailEQ(codexEmail), accountext.CodexAccountIDEQ(codexAccountID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: codex key not found in scope", ErrNotFound)
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}

// accountExtQuery ext 查询基座：作用域未注入（管理面全量）⇒ 不附加谓词；供应商面
// ⇒ 限定在「本归属账号」内——owner 谓词经**关联子查询（EXISTS）AND 进同一条 SQL**，
// 在 ext 查询**执行时**参与 WHERE。**不得**先执行 `accounts.IDs(ctx)` 再以物化旧
// id 集独立查 ext（那会把 service 的 TOCTOU 搬到 repository：两条语句之间转属即读
// 到他人 ext，违反 spec S:139/S:826）。
//
// **与账号单读同谓词**：不额外过滤软删——软删判据由调用方按行内 `deleted_at` 自行
// 决定（导入路径据此给出「账号已删除」的明确行级文案，而不是把已删行伪装成键冲突）。
func (r *AccountExtRepo) accountExtQuery(ctx context.Context) *ent.AccountExtQuery {
	q := r.client.AccountExt.Query()
	s := domain.AccountScopeFrom(ctx)
	if !s.Set {
		return q
	}
	// 归属集由子查询判定：命中 0 行（无归属账号）自然看不到任何 ext（fail-closed，
	// 与旧「空 id 集」同义，但不再有两次查询之间的转属窗口）。
	return q.Where(accountext.HasAccountWith(accountOwnerPred(s)...))
}

// accountOwnerFilter 把归属谓词 AND 进 accounts 上的 UPDATE（`WHERE id = $1 AND
// supplier_user_id = $2`）：影响行数 0 即代表「账号越域或 revision 陈旧」，两种
// 情形在调用方同一错误分支（不区分，避免泄漏他人行状态）。
func accountOwnerUpdate(ctx context.Context, u *ent.AccountUpdate) *ent.AccountUpdate {
	s := domain.AccountScopeFrom(ctx)
	if !s.Set {
		return u
	}
	return u.Where(account.SupplierUserID(s.OwnerUID))
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

// GetOwnedAccountExt 作用域内的 ext 单读（§2.5）：owner 谓词经**关联子查询
// （EXISTS）AND 进同一条 SQL**，在 ext 查询**执行时**参与 WHERE——越域 ⇒ 零行 ⇒
// ErrNotFound。不存在「先执行 accounts.IDs 取 owner 集合、再以物化旧 id 集独立查
// ext」的两查询窗口（第二条 WHERE 不含 owner 时，两条语句之间转属即读到他人凭据，
// 违反 spec S:139/S:826）。
// 无 ext 行（api-key）同样 ErrNotFound（调用方按「无上游能力」处理，与
// GetAccountExt 同族错误，不区分缺失原因以免泄漏归属）。
//
// 子查询带 `deleted_at IS NULL`：软删账号不可见（与账号读面同谓词）。
func (r *AccountExtRepo) GetOwnedAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	q := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(accountID))
	if s := domain.AccountScopeFrom(ctx); s.Set {
		q = q.Where(accountext.HasAccountWith(
			account.ID(accountID),
			account.SupplierUserID(s.OwnerUID),
			account.DeletedAtIsNil(),
		))
	}
	row, err := q.Only(ctx)
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
		return fmt.Errorf("%w: account_id=%d expected revision %d stale or outside scope", ErrStaleRevision, accountID, expectedRevision)
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
		return fmt.Errorf("%w: account_id=%d expected revision %d stale or outside scope", ErrStaleRevision, accountID, expectedRevision)
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
//
// **作用域边界（C1，spec §2.5）**：事务第一步即以 owner 谓词复核账号可见性——
// 越域/缺失 ⇒ ErrNotFound/404，且早于任何 ext 读取与写入。首写插入由本方法内
// 的 upsert（ON CONFLICT DO NOTHING 语义）完成，故「账号存在性 + CAS + ext 变更」
// 全在同一事务与同一 owner 谓词内（不再有服务面先 TryInsert 落引、后 CAS 拒绝的
// 转属窗口）。并发首写败者的 CAS 因 revision 不推进而失败并回滚，不会覆盖赢者身份。
func (r *AccountExtRepo) AdminUpsertAccountExtCAS(ctx context.Context, e *domain.AccountExt, expectedRevision int64) (*domain.AccountExt, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireOwnedAccountInTx(ctx, tx.Client(), e.AccountID); err != nil {
		return nil, err
	}
	cur, err := loadAccountExtInTx(ctx, tx.Client(), e.AccountID)
	if err != nil {
		return nil, err
	}
	advanceIdentity := extIdentityChanged(cur, e,
		extCredentialType, extIdentity, extOAuthToken, extOAuthRefresh,
		extOAuthExpires, extPATKey, extEmail, extCodexAccountID)
	n, err := bumpAccountGeneration(ctx, tx.Client(), e.AccountID, expectedRevision, advanceIdentity)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// 并发转属把账号移出当前作用域 ⇒ ErrNotFound（404）；否则 revision 陈旧。
		if oerr := requireOwnedAccountInTx(ctx, tx.Client(), e.AccountID); oerr != nil {
			return nil, oerr
		}
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
