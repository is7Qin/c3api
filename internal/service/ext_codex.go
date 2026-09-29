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

// GetAccountExt 账号 ext 行（编辑回显）。账号缺 id → 404。
func (s *Service) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	if _, err := s.store.GetAccount(ctx, accountID); err != nil {
		return nil, mapRepoErr(err)
	}
	e, err := s.store.GetAccountExt(ctx, accountID)
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

// UpsertAccountExt 幂等写入账号 ext 行。账号缺 id → 404。
// 类型一致性：ext 行 credential_type 必须与父行（账号所属模板）的
// credential_type 一致（账号无独立类型列，类型继承自模板）——不一致 → 400。
// 身份自动管理（持久身份现只含 installation_id）：无存量行 → NewCodexIdentity()
// 生成 installation_id 并经 TryInsert（ON CONFLICT DO NOTHING 先写者胜）原子首写
// ——并发双导入同一账号不覆盖不报错，冲突方完全采用赢者身份后走围栏 CAS 写令牌
// （方向 3：显式身份只在首写成功路径生效）；后续写入缺省 → 沿用存量（持久复用，
// 账号存在期间稳定）；调用方显式提供 → 采用。email 不在缺省沿用面——未提供 →
// NULL 清空（契约）。
// 校验先于落库：列组校验在 TryInsert 之前——被拒凭据零残留（400 前不写库）；
// 终校验保留（冲突路径重改 e 后，早校验覆盖不到）。
// 围栏写（d401b71）：终写必经 AdminUpsertAccountExtCAS（revision 原子递增，
// 无绕围栏面、无双增）。并发首写参与者全部预读同一 revision——围栏过期 ≠
// 内容冲突：CAS 败者重读最新 revision 与持久身份（漂移则再采用）后重试 CAS，
// 并发首写永不返回 conflict；revision 未推进的 conflict
// 原样上抛（非竞态冲突不重试，兼作活锁守卫）。
// 提交后即失效：ext 行是调度快照经 Selection.Ext 消费的 codex 凭据原料，
// 成功写入后按账号写面统一失效面做组级定向重载 + NOTIFY（快照重载幂等：值
// 相等即复用叶子，不打断在途计划，故无条件重载恒正确，无需按值判定）。
func (s *Service) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	acc, err := s.store.GetAccount(ctx, e.AccountID)
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
	orig := *e // 首写冲突回退用（丢弃本请求生成的未用身份，回到显式输入）
	cur, err := s.store.GetAccountExt(ctx, e.AccountID)
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
		// 首写原子性（I2）：ON CONFLICT DO NOTHING 先写者胜——冲突（并发已
		// 首写）→ 回读赢者完全采用其身份（方向 3：显式身份只在首写成功
		// 路径生效——败者派生值不得覆盖赢者，最终身份确定）
		inserted, ierr := s.store.TryInsertAccountExt(ctx, e)
		if ierr != nil {
			return nil, mapRepoErr(ierr)
		}
		if !inserted {
			winner, gerr := s.store.GetAccountExt(ctx, e.AccountID)
			if gerr != nil {
				return nil, mapRepoErr(gerr)
			}
			*e = orig
			e.CodexIdentity = winner.CodexIdentity // 完全采用赢者身份
			if e.CodexEmail == nil {
				e.CodexEmail = winner.CodexEmail // 未提供 email → 沿用赢者（管理标识随首写者）
			}
		}
	}
	// 终校验（冲突路径重改 e 后，早校验覆盖不到）——校验失败不落库
	if err := validateAccountExt(e); err != nil {
		return nil, err
	}
	// 围栏写：CAS revision 原子递增（身份已在 TryInsert 仲裁下定型，重试
	// 永不改身份来源——只换围栏令牌）。并发首写参与者全部预读同一 revision，
	// 先 CAS 者胜出、其余 stale——败者重读最新 revision 与持久行（身份漂移
	// 则完全再采用，防混搭）后重试，并发首写永不返回 conflict。revision 未
	// 推进的 conflict 非竞态（活锁守卫），原样上抛。
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
		row, rerr := s.store.GetAccountExt(ctx, e.AccountID)
		if rerr != nil && !errors.Is(rerr, repository.ErrNotFound) {
			return nil, mapRepoErr(rerr)
		}
		if rerr == nil && row.CodexIdentity != nil && *row.CodexIdentity != *e.CodexIdentity {
			e.CodexIdentity = row.CodexIdentity // 持久身份已漂移 → 完全采用
		}
	}
}
