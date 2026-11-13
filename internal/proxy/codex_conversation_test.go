// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

const (
	convNewTurn1 = `{"model":"gpt-4o","stream":true,"input":[{"role":"user","content":"first question"}]}`
	convNewTurn2 = `{"model":"gpt-4o","stream":true,"input":[{"role":"user","content":"first question"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]},{"role":"user","content":"second question"}]}`
	// convFollowUp1 是 convNewTurn1 那一轮的后续请求（以工具输出结尾，锚点不变）。
	convFollowUp1 = `{"model":"gpt-4o","stream":true,"input":[{"role":"user","content":"first question"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`
)

func convHdr(id string) map[string]string { return map[string]string{"Session-Id": id} }

// TestConversationSkipsSlotClaim（C3-3 依赖会话簿的部分）：有对话标识且会话簿已装配
// → 身份来自会话簿（Open 一次），与槽位身份不同、槽位池不推进；会话簿为 nil → 回到
// 槽位且身份非空；会话簿为 nil 且 MaxConcurrency=0（无池）→ 不 panic、按现状零身份。
func TestConversationSkipsSlotClaim(t *testing.T) {
	t.Run("book wired", func(t *testing.T) {
		up, u := newConvUpstream(t)
		book := newMiniredisBook(t, miniredis.RunT(t))
		p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10, maxConc: 1}}, book: book})

		code, body := postConv(t, p, "ck-1", convNewTurn1, nil)
		require.Equal(t, http.StatusOK, code, body)
		slot := u.wire(t, 0).identity(0)
		require.True(t, isUUIDv7(slot.metaThread), "无对话标识 → 槽位身份")

		code, body = postConv(t, p, "ck-1", convNewTurn1, convHdr("conv-a"))
		require.Equal(t, http.StatusOK, code, body)
		conv := u.wire(t, 1).identity(0)
		require.Equal(t, int64(1), book.Stats().Opens, "会话路径经会话簿 Open")
		require.NotEqual(t, slot.metaThread, conv.metaThread, "会话路径不用槽位身份")
		require.Equal(t, conv.threadHdr, conv.metaThread)

		// K=1：会话请求未认领槽位，下一个无对话标识请求仍是同一槽位线程。
		code, body = postConv(t, p, "ck-1", convNewTurn2, nil)
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, slot.metaThread, u.wire(t, 2).identity(0).metaThread)
		require.Equal(t, int64(1), book.Stats().Opens, "槽位路径不碰会话簿")
	})

	t.Run("nil book falls back to slot", func(t *testing.T) {
		up, u := newConvUpstream(t)
		p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10, maxConc: 1}}})
		code, body := postConv(t, p, "ck-1", convNewTurn1, convHdr("conv-a"))
		require.Equal(t, http.StatusOK, code, body)
		id := u.wire(t, 0).identity(0)
		require.True(t, isUUIDv7(id.metaThread), "会话簿为 nil → 槽位身份（非匿名）")
		require.Equal(t, id.metaThread, id.threadHdr)
		require.Equal(t, convInstallation(10), id.metaInstallation)
	})

	t.Run("nil book without pool", func(t *testing.T) {
		up, u := newConvUpstream(t)
		p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10, maxConc: -1}}})
		code, body := postConv(t, p, "ck-1", convNewTurn1, convHdr("conv-a"))
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, 1, u.n())
	})
}

// TestCodexConversationIdentityStable（C3-4）：同一对话两次 HTTP 请求 → 上游身份头、
// client_metadata 会话键、prompt_cache_key 相同；换 API key → 不同；同缓存域两个账号
// → 线程号相同、installation 不同。
func TestCodexConversationIdentityStable(t *testing.T) {
	mr := miniredis.RunT(t)
	book := newMiniredisBook(t, mr)
	up, u := newConvUpstream(t)
	p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10, domain: "team-a"}}, book: book})

	for i, body := range []string{convNewTurn1, convNewTurn2} {
		code, out := postConv(t, p, "ck-1", body, convHdr("conv-stable"))
		require.Equal(t, http.StatusOK, code, "req %d: %s", i, out)
	}
	a, b := u.wire(t, 0).identity(0), u.wire(t, 1).identity(0)
	require.True(t, isUUIDv7(a.threadHdr))
	for _, pair := range [][2]string{
		{a.sessionHdr, b.sessionHdr}, {a.threadHdr, b.threadHdr}, {a.windowHdr, b.windowHdr},
		{a.metaSession, b.metaSession}, {a.metaThread, b.metaThread}, {a.metaWindow, b.metaWindow},
		{a.promptCacheKey, b.promptCacheKey},
	} {
		require.NotEmpty(t, pair[0])
		require.Equal(t, pair[0], pair[1])
	}
	require.NotEqual(t, a.metaTurn, b.metaTurn, "新的一轮 → 新轮次号")

	// 换 API key：对话键不同 → 线程不同。
	code, out := postConv(t, p, "ck-2", convNewTurn1, convHdr("conv-stable"))
	require.Equal(t, http.StatusOK, code, out)
	c := u.wire(t, 2).identity(0)
	require.NotEqual(t, a.threadHdr, c.threadHdr, "换 API key → 不同对话")

	// 同缓存域的另一个账号（另一副本形态：共用同一会话簿）：线程号相同，installation 不同。
	p2 := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 11, domain: "team-a"}}, book: book})
	code, out = postConv(t, p2, "ck-1", convNewTurn2, convHdr("conv-stable"))
	require.Equal(t, http.StatusOK, code, out)
	d := u.wire(t, 3).identity(0)
	require.Equal(t, "Bearer "+convPAT(11), d.auth)
	require.Equal(t, a.threadHdr, d.threadHdr, "同域两账号共用线程号")
	require.Equal(t, a.metaThread, d.metaThread)
	require.Equal(t, convInstallation(10), a.metaInstallation)
	require.Equal(t, convInstallation(11), d.metaInstallation, "installation 按账号")
}
