// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import (
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// SelectionHandle 是控制面的私有租约/会话句柄：数据面对它只有这四个操作，
// 一个不多（F1 精确断言 len == 4，见 contract_gate_test.go）。
//
// 为什么恰好是这四个：
//   - Release：归还并发槽 + 会话（幂等，CAS 一次）。数据面在每次尝试收尾时调。
//   - Thread：本次请求的槽位身份（SDK 线程/会话四元组）；无会话为零值。
//   - StepIdentity：一次完成轮的会话推进（仅拿到 usage 且连接归还后调一次）。
//   - Credential：凭据投影（含来源身份代际 K）；nil Ext 恒为零值。
//
// 为什么 LogMappedModel/ClientResponseModel/PriceModel **不在这里**：它们只是
// Selection.Model + ModelMappingMode 的纯数据投影——不碰租约、不碰会话，把三个
// 签名塞进句柄只会白送冻结面（momus §3）。它们是 *Selection 上的纯函数。
//
// 实现由控制面提供（scheduler 的 selection 包装）；本包不 import 它（F2）。
type SelectionHandle interface {
	// Release 归还本次预留的并发槽与会话。幂等：第二次调用是 no-op。
	Release()
	// Thread 本次请求的槽位身份；无会话（非 codex / 池缺席 / 会话路径）为零值。
	Thread() sdkbridge.ThreadHandle
	// StepIdentity 推进本会话一次完成轮；无会话 no-op（退休动作在锁外发生）。
	StepIdentity()
	// Credential 本次请求的凭据投影：CredentialFromExt 各列 + 来源 K。
	// 零值 Selection（nil Ext）返回零值 Credential——早退语义与 P3b 前逐字一致。
	Credential() domain.AccountCredential
}

// detachedHandle 是「没有控制面租约」的句柄：四个方法全是 no-op / 零值。
//
// 它存在的唯一理由是保住 P3b 前的**nil receiver 语义**：当时 `(*Selection).Release`
// 等方法在 `sel == nil`（以及零值 Selection）上早退，数据面因此可以把「没有预留」
// 的 selection 丢进同一条收尾路径。形状 A 之后句柄挂在 Selection 上，这份语义
// 必须有个显式落点；它就是这个零尺寸值。
type detachedHandle struct{}

func (detachedHandle) Release()                       {}
func (detachedHandle) StepIdentity()                  {}
func (detachedHandle) Thread() sdkbridge.ThreadHandle { return sdkbridge.ThreadHandle{} }
func (detachedHandle) Credential() domain.AccountCredential {
	return domain.AccountCredential{}
}

var _ SelectionHandle = detachedHandle{}

// HandleOrDetached 取本次派发的控制面句柄；nil Selection / nil Handle ⇒ 分离句柄。
//
// **这是数据面对句柄的唯一取值口**：它把「nil receiver 早退」从 4 个方法各写一遍
// 收成一个判空点，同时让调用点仍然是**接口方法调用**（`sel.HandleOrDetached().Release()`）
// ——契约被真正消费，而不是被一个具体类型的方法转发掩盖。
//
// 零尺寸返回值不分配：detachedHandle{} 是 zerobase 上的静态值。
func (s *Selection) HandleOrDetached() SelectionHandle {
	if s == nil || s.Handle == nil {
		return detachedHandle{}
	}
	return s.Handle
}
