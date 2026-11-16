// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"encoding/hex"
	"fmt"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// TurnRecordKey 返回某次调用的**轮次记录在 Redis 里的键**：
//
//	codex:turn:<hex(对话键)>:<hex16(轮次键)>
//
// 为什么由 bridge 提供：这个布局是 SDK 存储层的内部格式（redisstore 里未导出），
// 宿主不得自行拼装——但运维诊断与"跨副本/跨缓存域归属"验证又必须能指着某条记录
// 说话。于是由最靠近 SDK 的这一层代拼，并由 `book_test.go` 用 miniredis 校验
// "拼出来的键就是会话簿真正写下的那个"（格式若变，那条测试先红）。
//
// 与 Book 内部用的是同一对输入：对话键由 (缓存域, API key 行号, 对话标识) 派生，
// 轮次键由 payload 的形状判定派生——所以调用方给的 payload 必须是**当次真正发出去的
// 那一份**，否则拼出来的是另一条记录。
func TurnRecordKey(cacheDomain string, apiKeyID int64, conversation string, payload []byte) string {
	k := codexsdk.NewConversationKey(cacheDomain, apiKeyID, conversation)
	turn := codexsdk.AnalyzeConversationInput(payload).TurnKey
	return fmt.Sprintf("codex:turn:%s:%016x", hex.EncodeToString(k[:]), turn)
}
