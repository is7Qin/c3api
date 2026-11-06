// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// egressNote 出口失败注解（spec-proxy-pool §5.1/§5.2）：本次尝试绑定了出口代理
// 时给出脱敏标识 `proxy_id=<id> <scheme://host:port>`，否则给空串。
//
// **为什么只出这几个字段**：`egress.Spec.String()` 只出 scheme/host/port，用户名
// 与密码恒不出；Spec 摘要（16 字节）也不落——§5.2 明令：短摘要对弱口令代理可
// 离线爆破。
//
// **为什么直连恒空**：`sel.ProxyID == nil` = 未绑定 ⇒ 零代理部署的 err_logs.message
// 与 M3 之前**逐字节**一致（零回归的一部分）。id 与 Egress 在同一个 Selection
// 字面量里同源取得，所以它指的就是本次尝试真正用的那条绑定。
func egressNote(sel *scheduler.Selection) string {
	if sel == nil || sel.ProxyID == nil || sel.Egress == nil {
		return ""
	}
	return "proxy_id=" + strconv.FormatInt(*sel.ProxyID, 10) + " " + sel.Egress.Spec.String()
}

// withEgressNote 把出口标识拼到网络失败文本尾部（`<原文> (proxy_id=7 socks5://h:p)`）。
// 原文为空时只留标识；直连 ⇒ 原样返回。**先给尾注预留预算、再截原文**（message 列有界，
// 详见下面实现处的理由）。
func withEgressNote(msg string, sel *scheduler.Selection) string {
	note := egressNote(sel)
	if note == "" {
		return domain.TruncateErrMsg(msg)
	}
	if msg == "" {
		return note
	}
	// **先截原文、给注解留位置**：拨号错误恰是最长的一类（带 URL/地址/嵌套 op 错误），
	// 而 message 列有界。若先拼接再整体截断，被切掉的正好是尾部的出口标识——那一段
	// 却是排障时唯一想知道的东西（"哪条出口挂了"）。按注解长度预留预算。
	suffix := " (" + note + ")"
	budget := domain.ErrMsgMaxLen - len([]rune(suffix))
	if budget <= 0 {
		return note // 注解本身撑满上限（真实规格到不了）：保标识在场
	}
	if r := []rune(msg); len(r) > budget {
		msg = string(r[:budget])
	}
	return msg + suffix
}
