// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package paymenterr 定义**支付域**的业务错误哨兵（单一真相）。
//
// 为什么单独成包（R1-c）：`handler/paymentadmin` 要为一个业务判定消耗
// `errors.Is`，却因此 import 整个存储实现层。方向必须是"实现 → 契约"。
//
// 为什么**不**放进 internal/storeerr（这条最关键，别把它改回去）：
// storeerr 的判据是"**任何**存储实现都会有的协议分类"——删掉业务语境后它还成立吗？
// `ErrNotFound` / `ErrConflict` / `ErrStaleIdentityRevision` 删掉业务语境依然成立，
// 所以它们在那里。本包的哨兵不成立：离开支付域，"merchant_ref 是谁的哪个字段"
// 就无从解释。更直接的证据是它的消费方：paymentadmin **故意**把它映射成 400
// 而不是 409——那是"你写了错误的配置"的业务策略，不是存储协议分类。把它塞进
// storeerr 会让那个包涨成收集业务错误的垃圾桶，且诱导下一批人照抄。
//
// 边界：只依赖 internal/storeerr（为了 `%w` 到 canonical ErrConflict，保住
// `errors.Is(err, ErrConflict)` 这条链）。**不**依赖 repository（否则环回来），
// **不**依赖 httpface（错误 → HTTP 状态码的翻译属于 handler 层）。
package paymenterr

import (
	"fmt"

	"github.com/is7qin/c3api/internal/storeerr"
)

// ErrMerchantRefImmutable 账户更新时新 config 推导出的 MerchantRef 与现列不符（N29）。
//
// 它**是** storeerr.ErrConflict 的细分（errors.Is 两者都为真）：数据库层能给的语义
// 就是冲突，但 spec §10-3 要求服务层映射 **400**（"改商户身份请新建账户 + 停用旧
// 账户"），故单列一个可识别的子哨兵免得服务层按字符串匹配。
//
// 为什么用 %w 而不是 errors.New：`errors.Is(err, storeerr.ErrConflict)` 必须成立
// （storeerr 是协议分类的那一侧），而调用方 paymentadmin.writeErr 要能认出这一个
// **具体**实例以便覆盖成 400。文案是契约：
// "repository: conflict: merchant_ref is read-only"——一字不改（含其前身自带的
// "repository: " 前缀，storeerr 搬家时保留了它，本单跟着保留）。
var ErrMerchantRefImmutable = fmt.Errorf("%w: merchant_ref is read-only", storeerr.ErrConflict)
