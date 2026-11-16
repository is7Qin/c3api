// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// CodexCredentialFromAccount 账号行 → 完整 codex 凭据（SDK 调用契约）。
//
// 单一实现（唯一一份凭据派生）：健康 probe（RuntimeHealth 的 PROBING 复检）与
// 429 额度补拉的 resolver **同源**——两处若各写一份，凭据字段/身份代际迟早
// 漂移，而 K 正是失效围栏与"旧结论不得作用到新身份"的判据。
//
// 返回 false 的三种情形（都不是错误，调用方按"跳过"处理）：账号行缺失、无模板、
// 模板类型不是 codex 族（api_key/responses-special 没有额度面）。
func CodexCredentialFromAccount(acct *domain.Account) (domain.AccountCredential, bool) {
	if acct == nil || acct.Template == nil {
		return domain.AccountCredential{}, false
	}
	if !acct.Template.CredentialType.IsCodex() {
		return domain.AccountCredential{}, false
	}
	cred := domain.CredentialFromExt(acct.Ext)
	// 账号行自带 ID / K 与凭据列**同一次快照读**（无 Selection 的调用面必须
	// 自己补上：K=0 会被失效链拒绝为"来源未知"）。
	cred.AccountID = acct.ID
	cred.IdentityRevision = acct.IdentityRevision
	return cred, true
}

// ResolveThrottleDeadline 实现 scheduler.QuotaResolver 契约（provider-agnostic
// worker 的 codex 侧落点）：把账号快照派生成完整凭据后，走 QuotaDeadline 的
// "缓存命中 → 窗口事实 → 卫生校验取最晚 reset"三段语义。
//
// 三分支口径（与 worker 的"不写 / 不写 / 不写"一致）：
//   - 非 codex 族 / 无凭据 ⇒ (zero,false,nil)：不是错误，本 resolver 无事可做；
//   - 补拉失败 ⇒ (zero,false,err)：错误面原样透传，由 worker 计 failed；
//   - 没有可信窗口 ⇒ (zero,false,nil)。
func (a *Codex) ResolveThrottleDeadline(ctx context.Context, acct *domain.Account) (time.Time, bool, error) {
	if a == nil {
		return time.Time{}, false, fmt.Errorf("sdkbridge: nil codex adapter")
	}
	cred, ok := CodexCredentialFromAccount(acct)
	if !ok {
		return time.Time{}, false, nil
	}
	return a.QuotaDeadline(ctx, &cred)
}
