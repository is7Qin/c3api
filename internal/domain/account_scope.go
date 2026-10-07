// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

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
