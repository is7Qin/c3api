// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "context"

// accountScopeCtxKey 账号作用域上下文键（单键单值）。作用域经 context 从供应商面
// 中间件（handler.SupplierScopeInject）透传到 repository 的账号读/写入口——每处
// WHERE 由 repository 读取该键并 AND 进谓词（§2.5：禁止「先按 id 取行再应用层比
// 归属」）。管理面/用户面不注入 ⇒ 缺省 = 管理面全量（PlatformAccountScope）。
type accountScopeCtxKey struct{}

// WithAccountScope 注入账号作用域（供应商面中间件调用）。
func WithAccountScope(ctx context.Context, s AccountScope) context.Context {
	return context.WithValue(ctx, accountScopeCtxKey{}, s)
}

// AccountScopeFrom 读取注入的账号作用域；缺省 = 管理面全量（{Set:false}）。
func AccountScopeFrom(ctx context.Context) AccountScope {
	if s, ok := ctx.Value(accountScopeCtxKey{}).(AccountScope); ok {
		return s
	}
	return PlatformAccountScope()
}

// AccountScope 账号读/写入口的行层作用域（spec 2026-10-09 §2.5）。供应商面恒传
// {OwnerUID: jwtUser, Set: true}，管理面传 {Set: false}（全量）。**作用域必须
// AND 进每一处 WHERE**（列表/单读/单改/删/批量/ext/recover/usage）——禁止「先按
// id 取行、再在应用层比归属」（TOCTOU）。
type AccountScope struct {
	// OwnerUID 归属过滤值（仅 Set=true 时生效）。
	OwnerUID int64
	// Set true = 供应商面归属作用域；false = 管理面全量。
	Set bool
}

// PlatformAccountScope 管理面全量作用域。
func PlatformAccountScope() AccountScope { return AccountScope{} }

// SupplierAccountScope 供应商面归属作用域（恒本人）。
func SupplierAccountScope(uid int64) AccountScope { return AccountScope{OwnerUID: uid, Set: true} }

// MatchesOwner 报告给定归属是否落在本作用域内（uid<=0 = 平台自有）。管理面
// （Set=false）恒真；供应商面仅本人归属为真。
func (s AccountScope) MatchesOwner(ownerUID int64) bool {
	if !s.Set {
		return true
	}
	return ownerUID == s.OwnerUID
}
