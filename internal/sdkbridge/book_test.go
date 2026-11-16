// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newMiniredisBook 生产同款构造路径（NewBook + redis.Scripter），只是把 Redis 换成
// miniredis。不变量：ContextTimeoutEnabled 必须为 true（redisstore 的每批预算依赖
// 它，缺了会在构造时 panic——这里顺带把那条约束钉在用例里）。
func newMiniredisBook(t *testing.T, mr *miniredis.Miniredis) *Book {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewBook(rdb)
}

// TestTurnRecordKeyPointsAtWrittenRecord TurnRecordKey 的格式必须与存储层真的写下的
// 那条记录一致。
//
// 这条用例存在的理由：`codex:turn:...` 的布局是 SDK 内部格式，本包只是**代拼**
// （见 book_diag.go）。代拼就有漂移风险，所以不比对常量，而是让会话簿真的写一次、
// 再检查拼出来的键确实存在——SDK 换格式时这里先红。
func TestTurnRecordKeyPointsAtWrittenRecord(t *testing.T) {
	mr := miniredis.RunT(t)
	book := newMiniredisBook(t, mr)
	payload := []byte(`{"model":"gpt-4o","input":[{"role":"user","content":"hi"}]}`)

	book.Identity(context.Background(), BookRequest{
		CacheDomain:    "cfg:1",
		APIKeyID:       1,
		ConversationID: "conv-x",
		InstallationID: "inst-1",
	}, payload)
	require.NoError(t, book.Flush(context.Background()))

	key := TurnRecordKey("cfg:1", 1, "conv-x", payload)
	require.True(t, mr.Exists(key), "TurnRecordKey 指向的键必须真的被写下；实际键集=%v", mr.Keys())
}

// TestBookRequestDerivesSameKeyAsSDK BookRequest 的派生必须与 SDK 同源：T1 把
// "三元组 → 对话键"与"payload → 形状"两段搬进本包时，唯一可能悄悄改掉的就是这里。
// 断言直接与 SDK 的两个函数对拍（不是与常量对拍——常量会跟着一起错）。
func TestBookRequestDerivesSameKeyAsSDK(t *testing.T) {
	payload := []byte(`{"model":"m","input":[{"role":"user","content":"hi"}]}`)
	req := BookRequest{
		CacheDomain:    "cfg:7",
		APIKeyID:       42,
		ConversationID: "conv-y",
		InstallationID: "inst-9",
		Owner:          [16]byte{1},
	}
	got := req.request(payload)

	require.Equal(t, codexsdk.NewConversationKey("cfg:7", 42, "conv-y"), got.Key,
		"对话键派生必须与 SDK 同源")
	require.Equal(t, "inst-9", got.InstallationID)
	require.Equal(t, [16]byte{1}, got.Owner)
	require.Equal(t, codexsdk.AnalyzeConversationInput(payload), got.Input,
		"payload 形状判定必须与 SDK 同源")
}
