// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package loadport

import "github.com/is7qin/c3api/internal/credential"

// ExtCredentialMismatch 一条被跳过的损坏账号：模板类型与 account_ext 行的类型
// BPS 位不一致（spec §0 唯一有行为意义的那一位）。
//
// 为什么住在装载端口而不是 domain：这是**装载期跨层观测**——"某个账号被装载器跳过
// 了"，是装载动作的结果，不是域事实。domain 已持有 pkg/egress 边且体量最大，继续
// 塞装载协议只会把它变成杂物间；internal/credential 只应承载凭据类型本身，不拥有
// "谁被跳过"。
//
// 字段、字段顺序、名字**逐字节搬自** internal/repository/group_repo.go 的旧定义，
// 一个方法都不加：它必须是可比较、可切片、可日志的值类型，且不允许任何人从这里
// 派生行为（装载语义仍在 repository）。
type ExtCredentialMismatch struct {
	AccountID    int64
	TemplateType credential.Type
	ExtType      credential.Type
}
