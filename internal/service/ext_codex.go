// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// —— 账号类型化鉴权扩展（account_ext 1:1；codex 专用——账号只两种 codex 类型，
// codex 列组：持久身份（codex_identity jsonb 单列——仅 installation_id）+
// codex_oauth_* 组 + codex_pat_key 组。数据层 CRUD + 契约） ——

// NewCodexIdentity 生成 codex 账号持久身份（账号导入时自动生成、持久复用；
// 纯函数零依赖——标准库 crypto/rand 构造 UUIDv4 形状）。持久身份现只含安装级
// installation_id（UUIDv4，~/.codex/installation_id 语义，账号级唯一身份）；
// 会话级 session/thread/window 已退役为运行时槽状态（scheduler 槽位池按完成轮数
// 演化），不再生成、不再持久化。
func NewCodexIdentity() domain.CodexIdentity {
	return domain.CodexIdentity{InstallationID: newUUIDv4()}
}

// newUUIDv4 生成 UUIDv4 字符串（16 随机字节，版本 4 + 变体 10 位）。
func newUUIDv4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("uuidv4: crypto/rand unavailable: %v", err)) // 永不发生（OS 熵源）
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b)
}

func formatUUID(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	for i := 0; i < 16; i++ {
		out[i*2] = hex[b[i]>>4]
		out[i*2+1] = hex[b[i]&0x0f]
	}
	// 插入连字符（8-4-4-4-12）：后 20 个 hex 字符右移 4 位
	copy(out[24:36], out[20:32])
	out[23] = '-'
	copy(out[19:23], out[16:20])
	out[18] = '-'
	copy(out[14:18], out[12:16])
	out[13] = '-'
	copy(out[9:13], out[8:12])
	out[8] = '-'
	return string(out)
}

// validateAccountExt 校验账号 ext 行：credential_type ∈ {codex-oauth, codex-pat}
// （类型白名单）；installation_id 必存（账号级唯一持久身份，service 自动生成
// 兜底）；oauth 只允许 codex_oauth_* 列组 + 最小完整性（至少 codex_oauth_token，
// refresh/expires 可空——refresh 未过期场景可缺）；pat 只允许 codex_pat_key。
// 身份由 service 维护（导入时生成、持久复用），不参与列组约束。
//
// 身份缺失（nil CodexIdentity / installation 空）→ 400（正确行为——loud）：
// 应用写路径恒带完整身份（自动生成/沿用），NULL 身份行仅手工 SQL 可达
// （损坏行）；拒绝而非静默写残缺身份——防止消费面组装残缺伪装身份。
func validateAccountExt(e *domain.AccountExt) error {
	if e.AccountID <= 0 {
		return ErrInvalidInput
	}
	if !e.CredentialType.ValidAccountExt() {
		return ErrInvalidInput
	}
	if e.CodexIdentity == nil || e.CodexIdentity.InstallationID == "" {
		return ErrInvalidInput
	}
	switch e.CredentialType {
	case credential.TypeCodexOAuth:
		if e.CodexPATKey != nil {
			return ErrInvalidInput
		}
		if e.CodexOAuthToken == nil {
			return ErrInvalidInput // oauth 组最小完整性：至少 codex_oauth_token
		}
	case credential.TypeCodexPAT:
		if e.CodexOAuthToken != nil || e.CodexOAuthRefreshToken != nil || e.CodexOAuthExpiresAt != nil {
			return ErrInvalidInput
		}
		if e.CodexPATKey == nil {
			return ErrInvalidInput // pat 组最小完整性（与 oauth 分支对称——空 key 写成功即死账号）
		}
	}
	return nil
}

// GetAccountExt 账号 ext 行（编辑回显）。账号缺 id / 越域 / 无 ext 行 → 404。
//
// **作用域边界（C1，spec §2.5）**：改用 scoped 单读 GetOwnedAccountExt——owner
// 谓词 AND 进**同一条 SQL**，越域 ⇒ 0 行 ⇒ 404；不再「先 GetAccount 再无作用域
// GetAccountExt」的两查询窗口（两次查询之间的转属 TOCTOU）。管理面（无作用域）
// 语义不变（GetOwnedAccountExt 在 {Set:false} 时退化为按 account_id 单读）。
func (s *Service) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	e, err := s.store.GetOwnedAccountExt(ctx, accountID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return e, nil
}

// fillIdentityDefaults 缺省身份沿用（持久复用）：installation 空 → 取存量。
// email 不在 fill 列表（未提供 → NULL 清空，兑现"全列更新含 NULL 清空"契约）。
// 调用方保证 cur 为存量行（已有行 carry-forward 或首写冲突赢者）。
func fillIdentityDefaults(e *domain.AccountExt, cur *domain.AccountExt) {
	if e.CodexIdentity == nil {
		if cur.CodexIdentity == nil {
			return
		}
		id := *cur.CodexIdentity // 值拷贝——不共享指针
		e.CodexIdentity = &id
		return
	}
	if cur.CodexIdentity == nil {
		return
	}
	if e.CodexIdentity.InstallationID == "" {
		e.CodexIdentity.InstallationID = cur.CodexIdentity.InstallationID
	}
}

// UpsertAccountExt 幂等写入账号 ext 行。账号缺 id / 越域 → 404。
// 类型一致性：ext 行 credential_type 必须与父行（账号所属模板）的
// credential_type 一致（账号无独立类型列，类型继承自模板）——不一致 → 400。
// 身份自动管理（持久身份现只含 installation_id）：无存量行 → NewCodexIdentity()
// 生成 installation_id，与凭据列一起经围栏 CAS 原子首写（ON CONFLICT DO NOTHING
// 语义落在写事务内，见 AdminUpsertAccountExtCAS）——并发双导入同一账号不覆盖、
// 不报错，冲突方完全采用赢者身份后重试（方向 3：显式身份只在首写成功路径生效）；
// 后续写入缺省 → 沿用存量（持久复用，账号存在期间稳定）；调用方显式提供 → 采用。
// email 不在缺省沿用面——未提供 → NULL 清空（契约）。
// 校验先于落库：列组校验在写事务之前——被拒凭据零残留（400 前不写库）。
//
// **作用域边界（C1，spec §2.5）**：账号存在性校验（store.GetAccount 走 scoped
// 单读）、存量 ext 读取（GetOwnedAccountExt，owner 谓词 AND 进同一 SQL）、CAS 与
// ext 变更（AdminUpsertAccountExtCAS 在同一事务/同一 owner 谓词内复核账号可见性
// 后再落 ext）三段都落在作用域谓词内，不再有「先取行再应用层判权」或两次查询
// 之间的转属 TOCTOU 窗口。
// 围栏写（d401b71）：终写必经 AdminUpsertAccountExtCAS（revision 原子递增，
// 无绕围栏面、无双增）。并发首写参与者全部预读同一 revision——围栏过期 ≠
// 内容冲突：CAS 败者重读最新 revision 与持久身份（漂移则再采用）后重试 CAS，
// 并发首写永不返回 conflict；revision 未推进的 conflict
// 原样上抛（非竞态冲突不重试，兼作活锁守卫）。
// 提交后即失效：ext 行是调度快照经 Selection.Ext 消费的 codex 凭据原料，
// 成功写入后按账号写面统一失效面做组级定向重载 + NOTIFY（快照重载幂等：值
// 相等即复用叶子，不打断在途计划，故无条件重载恒正确，无需按值判定）。
func (s *Service) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	acc, err := s.store.GetAccount(ctx, e.AccountID) // scoped 单读：越域/缺失 ⇒ 404
	if err != nil {
		return nil, mapRepoErr(err)
	}
	expectedRevision := acc.LifecycleRevision
	// 父模板类型 = 账号类型（账号无独立 credential_type 列）
	tpl, err := s.store.GetTemplate(ctx, acc.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != e.CredentialType {
		return nil, ErrInvalidInput // 父行（模板）类型与 ext 行类型必须一致
	}
	// 存量 ext 单读走 **scoped** 入口（C1）：owner 谓词 AND 进同一 SQL，越域不读
	// 他人 ext。无存量行与越域同族 ErrNotFound；越域在 store.GetAccount 已被 404
	// 拦截，故此处缺行即「首次创建」。
	cur, err := s.store.GetOwnedAccountExt(ctx, e.AccountID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, mapRepoErr(err) // 非缺行错误原样上抛（不误判为首次写入）
	}
	if err == nil {
		// 已有行：installation 缺省 → 沿用存量（持久复用）
		fillIdentityDefaults(e, cur)
	} else {
		// 无存量行：installation 缺省自动生成
		if e.CodexIdentity == nil {
			e.CodexIdentity = &domain.CodexIdentity{}
		}
		if e.CodexIdentity.InstallationID == "" {
			e.CodexIdentity.InstallationID = NewCodexIdentity().InstallationID
		}
		// 校验先于落库：自动生成值恒合法，早校验只命中列组违规
		if err := validateAccountExt(e); err != nil {
			return nil, err
		}
	}
	// 终校验（已有行路径沿用存量身份后）——校验失败不落库
	if err := validateAccountExt(e); err != nil {
		return nil, err
	}
	// 围栏写：CAS revision 原子递增，首写插入与 ext 变更都在写事务内完成（账号
	// 可见性在同一事务按作用域复核，越域 ⇒ 404，早于任何 ext 写入）。并发首写
	// 参与者全部预读同一 revision，先 CAS 者胜出、其余 stale——败者重读最新
	// revision 与持久行（身份漂移则完全再采用，防混搭）后重试，并发首写永不返回
	// conflict。revision 未推进的 conflict 非竞态（活锁守卫），原样上抛。
	rev := expectedRevision
	for {
		saved, werr := s.store.AdminUpsertAccountExtCAS(ctx, e, rev)
		if werr == nil {
			s.invalidateAccountLifecycle(ctx, saved.AccountID)
			return saved, nil
		}
		if !errors.Is(werr, repository.ErrConflict) {
			return nil, mapRepoErr(werr)
		}
		fresh, gerr := s.store.GetAccount(ctx, e.AccountID)
		if gerr != nil {
			return nil, mapRepoErr(gerr)
		}
		if fresh.LifecycleRevision <= rev {
			return nil, mapRepoErr(werr) // 围栏未推进：非并发首写竞态，conflict 原样上抛
		}
		rev = fresh.LifecycleRevision
		row, rerr := s.store.GetOwnedAccountExt(ctx, e.AccountID)
		if rerr != nil && !errors.Is(rerr, repository.ErrNotFound) {
			return nil, mapRepoErr(rerr)
		}
		if rerr == nil && row.CodexIdentity != nil && *row.CodexIdentity != *e.CodexIdentity {
			e.CodexIdentity = row.CodexIdentity // 持久身份已漂移 → 完全采用
		}
	}
}
