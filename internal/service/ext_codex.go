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
	"github.com/is7qin/c3api/internal/repository"
)

// —— 账号类型化鉴权扩展（account_ext 1:1；codex 专用——账号只两种 codex 类型，
// codex 列组：身份四元组（codex_identity jsonb 单列）+ codex_oauth_* 组 +
// codex_pat_key 组。W1 数据层 CRUD + 契约，消费接线 W6） ——

// NewCodexIdentity 生成 codex 账号身份四元组（账号导入时自动生成、持久复用；
// 纯函数零依赖——标准库 crypto/rand + time 构造 UUID 形状）：
//   - installation_id：UUIDv4（~/.codex/installation_id 语义，账号级唯一身份）；
//   - session_id / thread_id：UUIDv7（真实客户端主线程 thread_id==session_id，
//     同值对齐；UUIDv7 = 48bit unix ms + 版本位 + 随机位，时间有序近似）；
//   - window_id：{thread_id}:0（导入时生成后恒定不变——恒 0，用户裁决：高性能
//     网关不背透传解析（零分支零解析），上游不校验 n 单调性，形状正确即可）。
func NewCodexIdentity() domain.CodexIdentity {
	session := newUUIDv7(time.Now())
	return domain.CodexIdentity{
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
// oauth 只允许 codex_oauth_* 列组 + 最小完整性（至少 codex_oauth_token，
// refresh/expires 可空——refresh 未过期场景可缺）；pat 只允许 codex_pat_key。
// 身份四元组由 service 维护（导入时生成、持久复用），不参与列组约束。
//
// 身份缺失（nil CodexIdentity / installation 空）→ 400（正确行为——loud）：
// 应用写路径恒带完整身份（自动生成/沿用），NULL 身份行仅手工 SQL 可达
// （损坏行）；拒绝而非静默写残缺身份——防止消费面组装残缺伪装四元组。
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
// 空字段 = 未提供（codex_identity jsonb 契约：空串与缺省同形——identity 无
// 清空路径，账号存在期间稳定）——由调用方沿用存量/自动生成后兜底派生 window。
func normalizeCodexIdentity(e *domain.AccountExt, cur *domain.AccountExt) error {
	if e.CodexIdentity == nil {
		return nil // 全缺省：调用方自动生成/沿用
	}
	id := e.CodexIdentity
	// window 只给 → 反推 thread（{thread}:{n} 形状，剥最后 :段）
	if id.WindowID != "" && id.ThreadID == "" {
		w := id.WindowID
		i := strings.LastIndexByte(w, ':')
		if i <= 0 {
			return ErrInvalidInput // 形状非法：必须 {thread}:{n}
		}
		t := w[:i]
		if cur != nil && cur.CodexIdentity != nil && t != cur.CodexIdentity.ThreadID {
			return ErrInvalidInput // 存量行 window-only：反推 ≠ 存量 → 400
		}
		id.ThreadID = t
	}
	// thread==session 恒等：只给其一 → 补齐；成对显式冲突 → 拒绝
	switch {
	case id.SessionID == "" && id.ThreadID != "":
		id.SessionID = id.ThreadID
	case id.ThreadID == "" && id.SessionID != "":
		id.ThreadID = id.SessionID
	case id.SessionID != "" && id.ThreadID != "" && id.SessionID != id.ThreadID:
		return ErrInvalidInput
	}
	// window 恒 {thread}:0：thread 已知时显式 window 必须匹配
	if id.ThreadID != "" && id.WindowID != "" && id.WindowID != id.ThreadID+":0" {
		return ErrInvalidInput
	}
	return nil
}

// fillIdentityDefaults 缺省身份沿用（持久复用）：installation 空 → 取存量；
// session/thread 空 → 取存量。window 不沿用——恒 {thread}:0 派生（thread
// 定后由调用方兜底派生）。email 不在 fill 列表（B1-5：未提供 → NULL 清空，
// 兑现"全列更新含 NULL 清空"契约）。调用方保证 cur 为存量行（已有行
// carry-forward 或首写冲突赢者）。
func fillIdentityDefaults(e *domain.AccountExt, cur *domain.AccountExt) {
	if e.CodexIdentity == nil {
		if cur.CodexIdentity == nil {
			return
		}
		id := *cur.CodexIdentity // 值拷贝——不共享指针（后续兜底派生 window 不污染 cur）
		e.CodexIdentity = &id
		return
	}
	if cur.CodexIdentity == nil {
		return
	}
	if e.CodexIdentity.InstallationID == "" {
		e.CodexIdentity.InstallationID = cur.CodexIdentity.InstallationID
	}
	if e.CodexIdentity.SessionID == "" {
		e.CodexIdentity.SessionID = cur.CodexIdentity.SessionID
	}
	if e.CodexIdentity.ThreadID == "" {
		e.CodexIdentity.ThreadID = cur.CodexIdentity.ThreadID
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
// W1 不接线失效/发布。
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
	orig := *e // 首写冲突回退用（丢弃本请求生成的未用身份，回到显式输入）
	cur, err := s.store.GetAccountExt(ctx, e.AccountID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, mapRepoErr(err) // 非缺行错误原样上抛（不误判为首次写入）
	}
	// 先取存量行再归一（B1-3 方向 2）：存量行上 window-only 反推 ≠ 存量 → 400
	if err := normalizeCodexIdentity(e, cur); err != nil {
		return nil, err
	}
	if err == nil {
		// 已有行：身份字段缺省 → 沿用存量（持久复用；window 兜底派生）
		fillIdentityDefaults(e, cur)
	} else {
		// 无存量行：installation 缺省自动生成；会话三元组全缺省 → 自动生成
		if e.CodexIdentity == nil {
			e.CodexIdentity = &domain.CodexIdentity{}
		}
		id := e.CodexIdentity
		if id.InstallationID == "" {
			id.InstallationID = NewCodexIdentity().InstallationID
		}
		if id.SessionID == "" && id.ThreadID == "" && id.WindowID == "" {
			fresh := NewCodexIdentity()
			id.SessionID = fresh.SessionID
			id.ThreadID = fresh.ThreadID
			id.WindowID = fresh.WindowID
		}
		// window 恒 {thread}:0——thread 定后兜底派生（永不沿用旧 window）
		if id.ThreadID != "" && id.WindowID == "" {
			id.WindowID = id.ThreadID + ":0"
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
			e.CodexIdentity = winner.CodexIdentity // 完全采用赢者身份（单一完整四元组）
			if e.CodexEmail == nil {
				e.CodexEmail = winner.CodexEmail // 未提供 email → 沿用赢者（管理标识随首写者）
			}
		}
	}
	// window 恒 {thread}:0——thread 定后兜底派生（永不沿用旧 window）
	if e.CodexIdentity != nil && e.CodexIdentity.ThreadID != "" && e.CodexIdentity.WindowID == "" {
		e.CodexIdentity.WindowID = e.CodexIdentity.ThreadID + ":0"
	}
	// 终校验（B1-2：冲突路径重改 e 后，早校验覆盖不到）——校验失败不落库
	if err := validateAccountExt(e); err != nil {
		return nil, err
	}
	return s.store.UpsertAccountExt(ctx, e)
}
