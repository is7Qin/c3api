// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// —— 账号类型化鉴权扩展（account_ext 1:1；codex 专用——账号只两种 codex 类型，
// codex 列组：身份四元组（installation_id/session_id/thread_id/window_id）+
// oauth 组 + pat 组。W1 数据层 CRUD + 契约，消费接线 W6） ——

// CodexIdentity codex 账号身份四元组（对齐真实客户端语义：installation_id
// 安装级永久；session/thread 会话级；window = {thread_id}:{n} 起始 :0）。
type CodexIdentity struct {
	InstallationID string
	SessionID      string
	ThreadID       string
	WindowID       string
}

// NewCodexIdentity 生成 codex 账号身份四元组（账号导入时自动生成、持久复用；
// 纯函数零依赖——标准库 crypto/rand + time 构造 UUID 形状）：
//   - installation_id：UUIDv4（~/.codex/installation_id 语义，账号级唯一身份）；
//   - session_id / thread_id：UUIDv7（真实客户端主线程 thread_id==session_id，
//     同值对齐；UUIDv7 = 48bit unix ms + 版本位 + 随机位，时间有序近似）；
//   - window_id：{thread_id}:0（导入时生成后恒定不变——恒 0，用户裁决：高性能
//     网关不背透传解析（零分支零解析），上游不校验 n 单调性，形状正确即可）。
func NewCodexIdentity() CodexIdentity {
	session := newUUIDv7(time.Now())
	return CodexIdentity{
		InstallationID: newUUIDv4(),
		SessionID:      session,
		ThreadID:       session, // 主线程 thread_id == session_id（真实客户端语义）
		WindowID:       session + ":0",
	}
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

// newUUIDv7 生成 UUIDv7 字符串（时间有序：48bit unix ms + 版本 7 + 随机位 +
// 变体 10 位——真实客户端会话身份语义近似）。
func newUUIDv7(t time.Time) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("uuidv7: crypto/rand unavailable: %v", err))
	}
	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
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
// （类型白名单）；installation_id 必存（账号级唯一身份，service 自动生成兜底）；
// oauth 只允许 oauth_* 列组 + 最小完整性（至少 oauth_token，refresh/expires
// 可空——refresh 未过期场景可缺）；pat 只允许 pat_key。身份四元组由 service
// 维护（导入时生成、持久复用），不参与列组约束。
func validateAccountExt(e *domain.AccountExt) error {
	if e.AccountID <= 0 {
		return ErrInvalidInput
	}
	if !e.CredentialType.ValidAccountExt() {
		return ErrInvalidInput
	}
	if e.InstallationID == "" {
		return ErrInvalidInput
	}
	switch e.CredentialType {
	case credential.TypeCodexOAuth:
		if e.PATKey != nil {
			return ErrInvalidInput
		}
		if e.OAuthToken == nil {
			return ErrInvalidInput // oauth 组最小完整性：至少 oauth_token
		}
	case credential.TypeCodexPAT:
		if e.OAuthToken != nil || e.OAuthRefreshToken != nil || e.OAuthExpiresAt != nil {
			return ErrInvalidInput
		}
		if e.PATKey == nil {
			return ErrInvalidInput // pat 组最小完整性（B1-4：与 oauth 分支对称——空 key 写成功即死账号）
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

// normalizeCodexIdentity 身份恒等式归一（导入期；非运行时透传解析）：
//   - thread==session：只给其一 → 自动补齐恒等；成对显式冲突 → ErrInvalidInput；
//   - window 恒 {thread}:0（零透传）：只给 window → 剥尾段反推 thread；
//     显式 window ≠ {thread}:0 → ErrInvalidInput（window 永不落库为其它值）。
//   - 存量行（cur 非 nil）上只给 window：反推 ≠ 存量 thread → ErrInvalidInput
//     （B1-3 方向 2——派生值不得冒充显式值改身份：window-only 只允许与存量
//     一致的无操作轮换；无存量行才允许自由反推）。
//
// 缺失字段保持 nil——由调用方沿用存量/自动生成后兜底派生 window。
func normalizeCodexIdentity(e *domain.AccountExt, cur *domain.AccountExt) error {
	// window 只给 → 反推 thread（{thread}:{n} 形状，剥最后 :段）
	if e.WindowID != nil && e.ThreadID == nil {
		w := *e.WindowID
		i := strings.LastIndexByte(w, ':')
		if i <= 0 {
			return ErrInvalidInput // 形状非法：必须 {thread}:{n}
		}
		t := w[:i]
		if cur != nil && cur.ThreadID != nil && t != *cur.ThreadID {
			return ErrInvalidInput // 存量行 window-only：反推 ≠ 存量 → 400
		}
		e.ThreadID = &t
	}
	// thread==session 恒等：只给其一 → 补齐；成对显式冲突 → 拒绝
	switch {
	case e.SessionID == nil && e.ThreadID != nil:
		e.SessionID = e.ThreadID
	case e.ThreadID == nil && e.SessionID != nil:
		e.ThreadID = e.SessionID
	case e.SessionID != nil && e.ThreadID != nil && *e.SessionID != *e.ThreadID:
		return ErrInvalidInput
	}
	// window 恒 {thread}:0：thread 已知时显式 window 必须匹配
	if e.ThreadID != nil && e.WindowID != nil && *e.WindowID != *e.ThreadID+":0" {
		return ErrInvalidInput
	}
	return nil
}

// fillIdentityDefaults 缺省身份沿用（持久复用）：installation 空 → 取存量；
// session/thread nil → 取存量。window 不沿用——恒 {thread}:0 派生（thread
// 定后由调用方兜底派生）。email 不在 fill 列表（B1-5：未提供 → NULL 清空，
// 兑现"全列更新含 NULL 清空"契约）。调用方保证 cur 为存量行（已有行
// carry-forward 或首写冲突赢者）。
func fillIdentityDefaults(e *domain.AccountExt, cur *domain.AccountExt) {
	if e.InstallationID == "" {
		e.InstallationID = cur.InstallationID
	}
	if e.SessionID == nil {
		e.SessionID = cur.SessionID
	}
	if e.ThreadID == nil {
		e.ThreadID = cur.ThreadID
	}
}

// UpsertAccountExt 幂等写入账号 ext 行。账号缺 id → 404。
// 类型一致性：ext 行 credential_type 必须与父行（账号所属模板）的
// credential_type 一致（账号无独立类型列，类型继承自模板）——不一致 → 400。
// 身份恒等式（thread==session、window={thread}:0 零透传）：显式部分提供自动
// 补齐（normalizeCodexIdentity）；成对冲突 → 400；存量行上 window-only 反推
// ≠ 存量 thread → 400（B1-3 方向 2：派生值不得冒充显式值改身份）。
// 身份四元组自动管理：无存量行 → NewCodexIdentity() 生成四元组并经
// TryInsert（ON CONFLICT DO NOTHING 先写者胜）原子首写——并发双导入同一账号
// 不覆盖不报错，冲突方完全采用赢者身份后走普通 upsert 写令牌（B1-3 方向 3：
// 显式身份只在首写成功路径生效）；后续写入缺省 → 沿用存量（持久复用，账号
// 存在期间稳定）；调用方显式提供 → 采用。email 不在缺省沿用面——未提供 →
// NULL 清空（B1-5 契约）。
// 校验先于落库（B1-2）：window 派生 + 列组校验在 TryInsert 之前——被拒凭据
// 零残留（400 前不写库；含 NULL window 问题同步消除）；终校验保留（冲突路径
// 重改 e 后，早校验覆盖不到）。
func (s *Service) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	acc, err := s.store.GetAccount(ctx, e.AccountID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	// 父模板类型 = 账号类型（账号无独立 credential_type 列）
	tpl, err := s.store.GetTemplate(ctx, acc.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != e.CredentialType {
		return nil, ErrInvalidInput // 父行（模板）类型与 ext 行类型必须一致
	}
	cur, err := s.store.GetAccountExt(ctx, e.AccountID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, mapRepoErr(err) // 非缺行错误原样上抛（不误判为首次写入）
	}
	// The regular credential editor does not send codex_account_id. Preserve
	// the import-owned identifier unless an administrator explicitly supplies
	// a value (an empty value clears it).
	if e.CodexAccountID == nil && cur != nil {
		e.CodexAccountID = cur.CodexAccountID
	}
	orig := *e // 首写冲突回退用（丢弃本请求生成的未用身份，回到显式输入）
	// 先取存量行再归一（B1-3 方向 2）：存量行上 window-only 反推 ≠ 存量 → 400
	if err := normalizeCodexIdentity(e, cur); err != nil {
		return nil, err
	}
	if err == nil {
		// 已有行：身份字段缺省 → 沿用存量（持久复用；window 兜底派生）
		fillIdentityDefaults(e, cur)
	} else {
		// 无存量行：installation 缺省自动生成；会话三元组全缺省 → 自动生成
		if e.InstallationID == "" {
			e.InstallationID = NewCodexIdentity().InstallationID
		}
		if e.SessionID == nil && e.ThreadID == nil && e.WindowID == nil {
			id := NewCodexIdentity()
			e.SessionID = &id.SessionID
			e.ThreadID = &id.ThreadID
			e.WindowID = &id.WindowID
		}
		// window 恒 {thread}:0——thread 定后兜底派生（永不沿用旧 window）
		if e.ThreadID != nil && e.WindowID == nil {
			w := *e.ThreadID + ":0"
			e.WindowID = &w
		}
		// 校验先于首写落库（B1-2）：自动生成值恒合法，早校验只命中列组违规
		// （与已存在行校验语义无差异）——被拒凭据零残留
		if err := validateAccountExt(e); err != nil {
			return nil, err
		}
		// 首写原子性：ON CONFLICT DO NOTHING 先写者胜——冲突（并发已首写）
		// → 回读赢者完全采用其身份（B1-3 方向 3：显式身份只在首写成功路径
		// 生效——败者派生值不得覆盖赢者，最终身份确定）
		inserted, err := s.store.TryInsertAccountExt(ctx, e)
		if err != nil {
			return nil, mapRepoErr(err)
		}
		if !inserted {
			winner, gerr := s.store.GetAccountExt(ctx, e.AccountID)
			if gerr != nil {
				return nil, mapRepoErr(gerr)
			}
			*e = orig
			e.InstallationID = winner.InstallationID
			e.SessionID = winner.SessionID
			e.ThreadID = winner.ThreadID
			e.WindowID = winner.WindowID
			if e.Email == nil {
				e.Email = winner.Email // 未提供 email → 沿用赢者（管理标识随首写者）
			}
		}
	}
	// window 恒 {thread}:0——thread 定后兜底派生（永不沿用旧 window）
	if e.ThreadID != nil && e.WindowID == nil {
		w := *e.ThreadID + ":0"
		e.WindowID = &w
	}
	// 终校验（B1-2：冲突路径重改 e 后，早校验覆盖不到）——校验失败不落库
	if err := validateAccountExt(e); err != nil {
		return nil, err
	}
	saved, err := s.store.UpsertAccountExt(ctx, e)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.accountExtChanged(ctx, e.AccountID)
	return saved, nil
}

// accountExtChanged makes credential updates visible to the local scheduler
// and other instances. A full reload is safer than leaving stale credentials
// when the account-to-group lookup fails.
func (s *Service) accountExtChanged(ctx context.Context, accountID int64) {
	groups, err := s.store.GetAccountGroups(ctx, accountID)
	if err != nil {
		if s.log != nil {
			s.log.Warn("account groups query failed after credential update", logx.Int64("account_id", accountID), logx.Error(err))
		}
		if s.inv != nil {
			s.inv.Templates()
		}
		s.publish(ctx, notify.Change{Templates: true, Clients: true})
		return
	}
	if s.inv != nil {
		s.inv.Accounts(groups, true)
	}
	s.publish(ctx, notify.Change{Groups: groups, Clients: true})
}
