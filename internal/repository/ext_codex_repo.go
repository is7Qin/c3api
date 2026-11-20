// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/accountext"
)

// AccountExtRepo 账号类型化鉴权扩展（account_ext 1:1 边缘表；Codex/BPS
// provider-specific validation and shared identity envelope；Codex 列组：codex_identity
// （持久身份 jsonb，含 installation_id/BPS profile）+
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

func accountExtRow(ctx context.Context, tx *ent.Client, accountID int64) (*ent.AccountExt, error) {
	row, err := tx.AccountExt.Query().Where(accountext.AccountIDEQ(accountID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return row, err
}

// loadAccountExtInTx returns nil for a missing row and rejects corrupt projections.
func loadAccountExtInTx(ctx context.Context, tx *ent.Client, accountID int64) (*domain.AccountExt, error) {
	row, err := accountExtRow(ctx, tx, accountID)
	if err != nil || row == nil {
		return nil, err
	}
	return toDomainAccountExt(row)
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

// UpsertAccountExt is the existing transaction import entry point. For BPS it
// is insert-only and inside WithTx; replacement must use AdminUpsertAccountExtCAS.
func (r *AccountExtRepo) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	if e == nil {
		return nil, ErrInvalidInput
	}
	if e.CredentialType.IsBPS() {
		if _, inTx := r.driver.(*txDriver); !inTx {
			return nil, ErrProviderNotWired
		}
	}
	var saved *domain.AccountExt
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
		if err := lockOwnedAccountForUpdate(ctx, driver, e.AccountID); err != nil {
			return err
		}
		if err := validateAccountExtTemplate(ctx, client, e); err != nil {
			return err
		}
		cur, err := accountExtRow(ctx, client, e.AccountID)
		if err != nil {
			return err
		}
		if cur != nil && e.CredentialType.IsBPS() {
			return fmt.Errorf("%w: existing BPS credentials require a fenced write", ErrConflict)
		}
		identity, err := mergeIdentityForWrite(cur, e)
		if err != nil {
			return err
		}
		if err := upsertAccountExtRow(ctx, client, e, identity); err != nil {
			return err
		}
		saved, err = loadAccountExtInTx(ctx, client, e.AccountID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

func newAccountExtCreate(client *ent.Client, e *domain.AccountExt, identity *domain.IdentityEnvelope) *ent.AccountExtCreate {
	create := client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetNillableCodexOauthToken(e.CodexOAuthToken).
		SetNillableCodexOauthRefreshToken(e.CodexOAuthRefreshToken).
		SetNillableCodexOauthExpiresAt(e.CodexOAuthExpiresAt).
		SetNillableCodexPatKey(e.CodexPATKey).
		SetNillableCodexEmail(e.CodexEmail).
		SetNillableCodexAccountID(e.CodexAccountID)
	if identity != nil {
		create.SetCodexIdentity(identity)
	}
	return create
}

func upsertAccountExtRow(ctx context.Context, client *ent.Client, e *domain.AccountExt, identity *domain.IdentityEnvelope) error {
	_, err := newAccountExtCreate(client, e, identity).
		OnConflictColumns(accountext.FieldAccountID).
		Update(func(u *ent.AccountExtUpsert) {
			u.SetCredentialType(string(e.CredentialType))
			if identity != nil {
				u.SetCodexIdentity(identity)
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
	return err
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
	if e == nil {
		return false, ErrInvalidInput
	}
	var inserted bool
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, txDrv dialect.Driver) error {
		if err := lockOwnedAccountForUpdate(ctx, txDrv, e.AccountID); err != nil {
			return err
		}
		if err := validateAccountExtTemplate(ctx, client, e); err != nil {
			return err
		}
		identity, err := mergeIdentityForWrite(nil, e)
		if err != nil {
			return err
		}
		err = newAccountExtCreate(client, e, identity).
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
// **提交围栏（§3.3b-2 的"轮转回写"半边）**：`expectedIdentityRevision` 是产生
// 这次轮转的那版凭据的 K，谓词 `account.identity_revision = $K` 与更新在同一条
// SQL 里提交。管理面换凭据（K 推进）后，旧 Auth 在途 refresh 迟到的结果命中 0 行
// ⇒ 返回 ErrStaleIdentityRevision（**陈旧事件**，不是投递失败：调用方丢弃即可，
// 不得因此禁用账号）。
//
// 为什么围栏维度不是"旧 RT"：RT 每轮都换。多实例下同一账号的两个 Auth 各自轮转，
// DB 的 RT 由先到者写成新的——用旧 RT 作谓词会把**合法**轮转判成陈旧，让 DB 永久
// 停在已吊销的那一版。K 只由管理面身份写入推进、SDK 自刷新中性，才是正确判据。
//
// 行缺失（配置损坏——codex 账号必有 ext 行，选号前提）→ 0 行；回读区分后报
// ErrNotFound（fail-closed：错误 → SDK 回调重试 → 连续达阈值 CallbackDeliveryError
// fatal，管理员重新导入后恢复）。不做 INSERT 路径——回调无身份/类型材料（缺行场景
// = 配置损坏，应报错而非以空身份静默建行）。
func (r *AccountExtRepo) WriteOAuthRotation(ctx context.Context, accountID int64, expectedIdentityRevision int64, at, rt string, expiresAt *time.Time) error {
	u := r.client.AccountExt.Update().
		Where(accountext.AccountIDEQ(accountID),
			accountext.HasAccountWith(account.IdentityRevisionEQ(expectedIdentityRevision))).
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
		// 0 行回读区分（一次 PK 读，冷面）：ext 行仍在 ⇒ 围栏拒绝（K 已被推进）；
		// 缺行 ⇒ 配置损坏（返回既有 ErrNotFound 语义）。
		if _, gerr := r.GetAccountExt(ctx, accountID); gerr == nil {
			return fmt.Errorf("%w: account_id=%d rotation from stale identity revision %d",
				ErrStaleIdentityRevision, accountID, expectedIdentityRevision)
		}
		return fmt.Errorf("%w: account_id=%d ext row missing (codex account must have account_ext)", ErrNotFound, accountID)
	}
	return nil
}

// FindAccountExtByCodexKey 组合幂等键查重（批量导入专用——(codex_email,
// codex_account_id) 双条件 + 凭据身份组定位；GetAccountExt 仅按 account_id，
// 查重面不存在）。命中返回行（含 credential_type——跨类型判定用）；缺行 →
// ErrNotFound。
//
// **身份组（§2.5）**：同一个 ChatGPT 账号可以在 Codex 与 BPS 两个独立 provider
// 下各存在一行（不同 endpoint / 配额 / profile），故查重必须按
// credential.IdentityGroup() 收窄：BPS 只与 BPS 去重，Codex 仍在 oauth/pat
// 之间互斥（0002 的两条部分唯一索引在同一层面兜底）。组内优先返回与本次凭据
// 同类型的行；否则返回组内最早行，由调用方按 `cur.CredentialType != credType`
// 判跨类型冲突。
//
// **作用域（§2.5）**：供应商面（ctx 带作用域）时 owner 谓词经子查询 AND 进**同
// 一条 SQL**——他人归属的同键行查不到，于是不会走「先全局命中、再应用层比归属」
// 的 TOCTOU 路径（那正是 spec 明令禁止的形状）。跨归属同键随后撞全局唯一索引
// （23505）⇒ 行级 failed，不改他人行（调用方把它翻成不透出他人 id 的文案）。
func (r *AccountExtRepo) FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string, credType credential.Type) (*domain.AccountExt, error) {
	group := credType.IdentityGroup()
	types := make([]string, 0, len(group))
	for _, t := range group {
		types = append(types, string(t))
	}
	rows, err := r.accountExtQuery(ctx).
		Where(
			accountext.CodexEmailEQ(codexEmail),
			accountext.CodexAccountIDEQ(codexAccountID),
			accountext.CredentialTypeIn(types...),
		).
		Order(ent.Asc(accountext.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: codex key not found in scope", ErrNotFound)
	}
	for _, row := range rows {
		if credential.Type(row.CredentialType) == credType {
			return toDomainAccountExt(row)
		}
	}
	return toDomainAccountExt(rows[0])
}

var errInvalidBPSAccountExt = errors.New("repository: invalid stored BPS credentials")

// mergeIdentityForWrite mutates only the field owned by this credential type.
// Read the original envelope under the account lock, not from caller projections.
func mergeIdentityForWrite(cur *ent.AccountExt, next *domain.AccountExt) (*domain.IdentityEnvelope, error) {
	var env domain.IdentityEnvelope
	if cur != nil && cur.CodexIdentity != nil {
		env = *cur.CodexIdentity
	}
	if next.CredentialType.IsBPS() {
		profile := next.BPSProfile
		if profile == nil {
			profile = env.BPSProfile()
		}
		candidate := *next
		candidate.BPSProfile = profile
		if err := validateBPSExt(&candidate); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
		}
		if next.BPSProfile != nil {
			merged, err := env.ReplaceBPSProfile(profile)
			return &merged, err
		}
	} else if next.CodexIdentity != nil {
		merged, err := env.MergeCodexInstallation(next.CodexIdentity.InstallationID)
		return &merged, err
	} else if _, present := env["installation_id"]; present {
		// Keep the existing nil-CodexIdentity clear contract, but clear only
		// the Codex-owned field, never BPS or future keys.
		env = env.Clone()
		delete(env, "installation_id")
		if len(env) == 0 {
			return nil, nil
		}
	}
	if env == nil {
		return nil, nil
	}
	return &env, nil
}

func validateBPSExt(ext *domain.AccountExt) error {
	if err := domain.ValidateBPSProfileJSON(ext.BPSProfile); err != nil {
		return errors.New("BPS profile must be a non-null JSON object")
	}
	if strings.TrimSpace(derefString(ext.CodexOAuthToken)) == "" ||
		strings.TrimSpace(derefString(ext.CodexOAuthRefreshToken)) == "" ||
		strings.TrimSpace(derefString(ext.CodexAccountID)) == "" || ext.CodexPATKey != nil {
		return errors.New("BPS requires OAuth credentials and upstream account ID, without a PAT")
	}
	return nil
}

func identityEnvelopeChanged(cur *ent.AccountExt, next *domain.IdentityEnvelope) (bool, error) {
	var old *domain.IdentityEnvelope
	if cur != nil {
		old = cur.CodexIdentity
	}
	before, err := json.Marshal(old)
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	return !domain.IdentityJSONEqual(before, after), nil
}

func validateAccountExtTemplate(ctx context.Context, client *ent.Client, ext *domain.AccountExt) error {
	tpl, err := client.Account.Query().Where(account.IDEQ(ext.AccountID)).QueryTemplate().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return fmt.Errorf("%w: account %d has no template", ErrNotFound, ext.AccountID)
		}
		return err
	}
	ct := credential.Type(tpl.CredentialType)
	// Legacy Codex ext rows predate template credential typing and are allowed
	// to retain their existing method contract. BPS is strict: never write BPS
	// credentials through a non-BPS template (or a non-BPS ext through BPS).
	if ct.IsBPS() != ext.CredentialType.IsBPS() {
		return fmt.Errorf("%w: account template credential type %q does not match BPS extension %q", ErrInvalidInput, ct, ext.CredentialType)
	}
	return nil
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
	return toDomainAccountExt(row)
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
	return toDomainAccountExt(row)
}

// accountExtProjectionColumns ext 投影的列清单：额度面凭据投影与调度器快照共用
// 一份（表别名固定为 `e`，顺序即扫描顺序——改动必须同步 scanAccountExt）。
const accountExtProjectionColumns = `e.account_id, e.credential_type, e.codex_identity,
       e.codex_oauth_token, e.codex_oauth_refresh_token, e.codex_oauth_expires_at,
       e.codex_pat_key, e.codex_email, e.codex_account_id`

// nullStrPtr 可空文本列 → 指针（NULL 与空串在领域模型里都是"未提供"）。
func nullStrPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	raw := v.String
	return &raw
}

// scanAccountExtRevision 按 accountExtProjectionColumns 的列序读一行 ext 投影，
// 并读出紧随其后的 `a.identity_revision`（两条使用方都按"投影列 + 父行 K"选列）。
// codex_identity 是 jsonb：NULL → nil，坏 json → 报错原样上抛（与 ent 生成层扫描器
// 同语义）。
func scanAccountExtRevision(rows *entsql.Rows) (*domain.AccountExt, int64, error) {
	var (
		ext              domain.AccountExt
		credentialType   string
		identity         []byte
		oauthToken       sql.NullString
		oauthRefresh     sql.NullString
		oauthExpires     sql.NullTime
		patKey           sql.NullString
		email            sql.NullString
		codexAccountID   sql.NullString
		identityRevision int64
	)
	if err := rows.Scan(&ext.AccountID, &credentialType, &identity, &oauthToken, &oauthRefresh,
		&oauthExpires, &patKey, &email, &codexAccountID, &identityRevision); err != nil {
		return nil, 0, err
	}
	ext.CredentialType = credential.Type(credentialType)
	ext.CodexOAuthToken = nullStrPtr(oauthToken)
	ext.CodexOAuthRefreshToken = nullStrPtr(oauthRefresh)
	ext.CodexPATKey = nullStrPtr(patKey)
	ext.CodexEmail = nullStrPtr(email)
	ext.CodexAccountID = nullStrPtr(codexAccountID)
	if oauthExpires.Valid {
		t := oauthExpires.Time
		ext.CodexOAuthExpiresAt = &t
	}
	var env *domain.IdentityEnvelope
	if raw := bytes.TrimSpace(identity); len(raw) != 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			if ext.CredentialType.IsBPS() {
				return nil, 0, fmt.Errorf("%w: account_id=%d: invalid identity envelope", errInvalidBPSAccountExt, ext.AccountID)
			}
			return nil, 0, fmt.Errorf("account_id=%d codex_identity decode: %w", ext.AccountID, err)
		}
	}
	if err := projectAccountIdentity(&ext, env); err != nil {
		return nil, 0, err
	}
	return &ext, identityRevision, nil
}

// GetOwnedAccountCredential 额度面（无 Selection）的凭据投影（§2.5）：**一条
// SQL** 同时读出凭据列与父账号行的 `identity_revision`——`account_exts` JOIN
// `accounts`，owner 谓词 AND 在同一语句的 WHERE 里。
//
// 为什么必须一条语句：分两次读（先 ext 取凭据内容、再 accounts 取 K）时，两条
// 语句之间可能夹进一次管理面凭据替换 ⇒ 读出"旧内容 + 新 K"，这份凭据建出的条目
// 上的轮转/判死就能通过新身份的围栏（覆盖管理员刚换上的凭据、或让旧判决禁用新
// 身份）。JOIN 读出的两半来自同一快照，结构上没有这个窗口。
//
// 越域 / 账号缺失 / 无 ext 行 ⇒ ErrNotFound（调用方按"无上游能力"处理，不区分
// 缺失原因以免泄漏归属）。
func (r *AccountExtRepo) GetOwnedAccountCredential(ctx context.Context, accountID int64) (*domain.AccountCredential, error) {
	query := `SELECT ` + accountExtProjectionColumns + `, a.identity_revision
FROM account_exts e JOIN accounts a ON a.id = e.account_id
WHERE e.account_id = $1`
	args := []any{accountID}
	if s := domain.AccountScopeFrom(ctx); s.Set {
		args = append(args, s.OwnerUID)
		query += fmt.Sprintf(" AND a.supplier_user_id = $%d AND a.deleted_at IS NULL", len(args))
	}
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck // Rows.Err reports iteration failures.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: account_id=%d missing or outside scope", ErrNotFound, accountID)
	}
	ext, identityRevision, err := scanAccountExtRevision(rows)
	if err != nil {
		return nil, err
	}
	// 凭据组装与类型分发只有一处（CredentialFromExt）；这里补上同一次读出的 K。
	cred := domain.CredentialFromExt(ext)
	cred.IdentityRevision = identityRevision
	return &cred, nil
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
	if e == nil {
		return nil, ErrInvalidInput
	}
	var saved *domain.AccountExt
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
		if err := lockOwnedAccountForUpdate(ctx, driver, e.AccountID); err != nil {
			return err
		}
		if err := validateAccountExtTemplate(ctx, client, e); err != nil {
			return err
		}
		cur, err := accountExtRow(ctx, client, e.AccountID)
		if err != nil {
			return err
		}
		identity, err := mergeIdentityForWrite(cur, e)
		if err != nil {
			return err
		}
		identityChanged, err := identityEnvelopeChanged(cur, identity)
		if err != nil {
			return err
		}
		advanceIdentity := identityChanged || extIdentityChanged(accountExtColumns(cur), e,
			extCredentialType, extOAuthToken, extOAuthRefresh, extOAuthExpires, extPATKey, extEmail, extCodexAccountID)
		n, err := bumpAccountGeneration(ctx, client, e.AccountID, expectedRevision, advanceIdentity)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: account_id=%d expected revision %d stale", ErrStaleRevision, e.AccountID, expectedRevision)
		}
		if err := upsertAccountExtRow(ctx, client, e, identity); err != nil {
			return err
		}
		saved, err = loadAccountExtInTx(ctx, client, e.AccountID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}
