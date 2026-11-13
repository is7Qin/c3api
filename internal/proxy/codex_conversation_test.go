// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
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

// TestCodexConversationCrossReplica（C3-5）：两个 Proxy 实例（两份会话簿）共用一个
// miniredis：首请求到 A 并落地写入，同一轮的后续请求到 B → 上游收到同一轮次号与
// A 那次上游签发的 turn-state。
func TestCodexConversationCrossReplica(t *testing.T) {
	mr := miniredis.RunT(t)
	bookA, bookB := newMiniredisBook(t, mr), newMiniredisBook(t, mr)
	up, u := newConvUpstream(t)
	accs := []convTestAcc{{id: 10}}
	pA := newConvTestProxy(t, up.URL, convTestOpts{accs: accs, book: bookA})
	pB := newConvTestProxy(t, up.URL, convTestOpts{accs: accs, book: bookB})

	code, out := postConv(t, pA, "ck-1", convNewTurn1, convHdr("conv-x"))
	require.Equal(t, http.StatusOK, code, out)
	flushBook(t, bookA)

	code, out = postConv(t, pB, "ck-1", convFollowUp1, convHdr("conv-x"))
	require.Equal(t, http.StatusOK, code, out)
	first, follow := u.wire(t, 0).identity(0), u.wire(t, 1).identity(0)
	require.Empty(t, first.turnState, "新的一轮不带 turn-state")
	require.Equal(t, "ts-0", follow.turnState, "B 从共享存储取回 A 捕获的 turn-state")
	require.Equal(t, first.metaTurn, follow.metaTurn, "同一轮次号")
	require.Equal(t, first.threadHdr, follow.threadHdr, "同一线程")
	require.Equal(t, int64(1), bookB.Stats().Loads, "B 本地未命中 → 一次 Load")
}

// TestCodexConversationLocalHitZeroRedis（C3-14）：本副本命中时（对话与轮次都在本地
// 缓存），Prepare 与上游调用期间 miniredis 命令数差值为 0（不含 Flush）。
func TestCodexConversationLocalHitZeroRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	book := newMiniredisBook(t, mr)
	up, u := newConvUpstream(t)
	p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10}}, book: book})

	code, out := postConv(t, p, "ck-1", convNewTurn1, convHdr("conv-hot"))
	require.Equal(t, http.StatusOK, code, out)
	flushBook(t, book)

	before := mr.CommandCount()
	require.Positive(t, before, "首请求 Open 与 Flush 走了 Redis（计数面有效）")
	for _, body := range []string{convFollowUp1, convNewTurn2} {
		code, out = postConv(t, p, "ck-1", body, convHdr("conv-hot"))
		require.Equal(t, http.StatusOK, code, out)
	}
	require.Equal(t, before, mr.CommandCount(), "本副本命中零 Redis")
	require.Equal(t, "ts-0", u.wire(t, 1).identity(0).turnState, "后续请求取本地 turn-state")
	require.Equal(t, u.wire(t, 0).identity(0).threadHdr, u.wire(t, 2).identity(0).threadHdr)
}

// TestCodexConversationFailoverAcrossDomains（C3-16）：首个账号（域 d1）429 → 故障转移
// 到域 d2 的账号：第二次尝试按当次 sel 重新 Prepare，线程号不同；第二次上游签发的
// turn-state 只写进 d2 的轮次记录，d1 的轮次记录不被写入 turn-state。
func TestCodexConversationFailoverAcrossDomains(t *testing.T) {
	testHealthSink.reset()
	mr := miniredis.RunT(t)
	book := newMiniredisBook(t, mr)
	up, u := newConvUpstream(t, http.StatusTooManyRequests, http.StatusOK)
	p := newConvTestProxy(t, up.URL, convTestOpts{
		accs:  []convTestAcc{{id: 10, domain: "d1"}, {id: 20, domain: "d2"}},
		order: []int64{10, 20},
		book:  book,
	})
	code, out := postConv(t, p, "ck-1", convNewTurn1, convHdr("conv-fo"))
	require.Equal(t, http.StatusOK, code, out)
	require.Equal(t, 2, u.n())
	a, b := u.wire(t, 0).identity(0), u.wire(t, 1).identity(0)
	require.Equal(t, "Bearer "+convPAT(10), a.auth)
	require.Equal(t, "Bearer "+convPAT(20), b.auth)
	require.NotEqual(t, a.threadHdr, b.threadHdr, "跨缓存域 → 不同对话键、不同线程")
	require.Equal(t, convInstallation(20), b.metaInstallation)
	flushBook(t, book)

	turn := codexsdk.AnalyzeConversationInput([]byte(convNewTurn1)).TurnKey
	require.NotZero(t, turn)
	turnKey := func(domain string) string {
		k := codexsdk.NewConversationKey(domain, 1, "conv-fo")
		return fmt.Sprintf("codex:turn:%s:%016x", hex.EncodeToString(k[:]), turn)
	}
	require.True(t, mr.Exists(turnKey("d2")))
	require.Equal(t, "ts-1", mr.HGet(turnKey("d2"), "ts"), "成功尝试的 turn-state 归 d2")
	require.Empty(t, mr.HGet(turnKey("d1"), "ts"), "原域轮次记录不被写入 turn-state")
}

const convWSFrame1 = `{"type":"response.create","model":"gpt-4o","input":[{"role":"user","content":"ws question"}]}`

// requireHandshakeMatchesFrame 握手 turn-metadata / 线程头与该连接首帧的 client_metadata
// 一致（握手按首帧计算）。
func requireHandshakeMatchesFrame(t *testing.T, id convIdentity) {
	t.Helper()
	require.NotEmpty(t, id.turnMeta, "握手带 x-codex-turn-metadata")
	require.Equal(t, id.metaTurn, gjson.Get(id.turnMeta, "turn_id").String())
	require.Equal(t, id.metaThread, gjson.Get(id.turnMeta, "thread_id").String())
	require.Equal(t, id.threadHdr, id.metaThread)
	require.Equal(t, "model="+id.model, id.routingHint, "routing-hint 按首帧模型")
}

// TestCodexWSConversationFirstFrame（C3-6）：WS 会话路径 WithConversation 的首帧与实际
// 首帧逐字节相同（Send 复用 Dial 的计算，会话簿只 Prepare 一次）；故障转移换号且模型
// 映射不同时，第二次尝试的握手与首帧都按第二个账号的映射计算。
func TestCodexWSConversationFirstFrame(t *testing.T) {
	hdr := http.Header{"Session-Id": {"ws-conv"}}
	t.Run("single attempt", func(t *testing.T) {
		book := newMiniredisBook(t, miniredis.RunT(t))
		up, u := newConvUpstream(t)
		p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10}}, book: book})
		wsConvSession(t, p, hdr, convWSFrame1)

		w := u.wire(t, 0)
		require.True(t, w.ws)
		id := w.identity(0)
		requireHandshakeMatchesFrame(t, id)
		require.Equal(t, "gpt-4o", id.model)
		st := book.Stats()
		require.Equal(t, int64(1), st.ConversationCacheMisses)
		require.Zero(t, st.ConversationCacheHits, "首帧与 Dial 的 first 逐字节相同 → Send 不再 Prepare")
		require.Equal(t, int64(1), st.Opens)
	})

	t.Run("failover with different mapping", func(t *testing.T) {
		testHealthSink.reset()
		book := newMiniredisBook(t, miniredis.RunT(t))
		up, u := newConvUpstream(t, http.StatusTooManyRequests, http.StatusOK)
		p := newConvTestProxy(t, up.URL, convTestOpts{
			accs:  []convTestAcc{{id: 10, domain: "dd", mapped: "gpt-5.4"}, {id: 20, domain: "dd", mapped: "gpt-5.5"}},
			order: []int64{10, 20},
			book:  book,
		})
		wsConvSession(t, p, hdr, convWSFrame1)

		require.Equal(t, 2, u.n())
		first, second := u.wire(t, 0).identity(0), u.wire(t, 1).identity(0)
		require.Equal(t, "Bearer "+convPAT(10), first.auth)
		require.Equal(t, "model=gpt-5.4", first.routingHint, "首拨按账号 10 的映射")
		require.Equal(t, "Bearer "+convPAT(20), second.auth)
		require.Equal(t, "gpt-5.5", second.model, "第二个账号实际 Send 的首帧按其映射改写")
		requireHandshakeMatchesFrame(t, second)
		st := book.Stats()
		require.Equal(t, int64(1), st.ConversationCacheMisses)
		require.Equal(t, int64(1), st.ConversationCacheHits, "两次 Dial 各 Prepare 一次；Send 复用（首帧与拨号所用逐字节相同）")
	})
}

// TestCodexWSSlotHandshakeHeaders（C3-7 WS）：无对话标识 → 槽位路径（会话簿不参与），
// 握手带按首帧计算的 routing-hint 与 turn-metadata（WithFirstFrame）。
func TestCodexWSSlotHandshakeHeaders(t *testing.T) {
	book := newMiniredisBook(t, miniredis.RunT(t))
	up, u := newConvUpstream(t)
	p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10, mapped: "gpt-5.4"}}, book: book})
	wsConvSession(t, p, nil, convWSFrame1)

	id := u.wire(t, 0).identity(0)
	requireHandshakeMatchesFrame(t, id)
	require.Equal(t, "gpt-5.4", id.model)
	require.True(t, isUUIDv7(id.metaThread))
	require.Equal(t, convInstallation(10), id.metaInstallation)
	require.Equal(t, codexsdk.ConversationStats{}, book.Stats(), "槽位路径不碰会话簿")
}

// TestCodexSlotPathStillWorks（C3-7 HTTP/WS）：会话簿已装配但无对话标识 → HTTP 与 WS
// 都走槽位，请求为新形态（HTTP 体 zstd、client_metadata 由 SDK 合成）；K=1 时相邻
// 请求复用同一槽位线程。
func TestCodexSlotPathStillWorks(t *testing.T) {
	book := newMiniredisBook(t, miniredis.RunT(t))
	up, u := newConvUpstream(t)
	p := newConvTestProxy(t, up.URL, convTestOpts{accs: []convTestAcc{{id: 10, maxConc: 1}}, book: book})

	code, out := postConv(t, p, "ck-1", convNewTurn1, nil)
	require.Equal(t, http.StatusOK, code, out)
	wsConvSession(t, p, nil, convWSFrame1)
	code, out = postConv(t, p, "ck-1", convFollowUp1, nil)
	require.Equal(t, http.StatusOK, code, out)

	h0, ws, h2 := u.wire(t, 0), u.wire(t, 1), u.wire(t, 2)
	require.Equal(t, "zstd", h0.header.Get("Content-Encoding"), "HTTP 体由 SDK 整形并 zstd 压缩")
	a := h0.identity(0)
	require.True(t, isUUIDv7(a.metaThread))
	require.Equal(t, a.threadHdr, a.metaThread)
	require.Equal(t, a.metaThread, ws.identity(0).metaThread, "K=1：同一槽位线程")
	require.Equal(t, a.metaThread, h2.identity(0).metaThread)
	require.Empty(t, h2.identity(0).turnState, "槽位路径不带 turn-state")
	require.Equal(t, codexsdk.ConversationStats{}, book.Stats(), "槽位路径不碰会话簿")
}

// TestCodexClientIdentityNeverEgress（C3-12）：客户端头与体里的对话标识、
// client_metadata、prompt_cache_key 哨兵不出现在假上游收到的任何字节里（头 + zstd
// 解压后的 HTTP 体 + WS 帧），也不以原文进入 Redis（会话簿落盘后遍历 miniredis 全部
// 键与值）。三种传输（HTTP→HTTP、HTTP→WS、WS→WS）+ 两条协议转换车道（chat→resp、
// messages→resp）；另有“无头、标识只来自体键”的 HTTP 与 WS 车道。会话路径与槽位
// 路径（会话簿 nil）各跑一遍。
func TestCodexClientIdentityNeverEgress(t *testing.T) {
	sentinels := []string{"SENTINEL-HDR-THREAD", "SENTINEL-HDR-SESS", "SENTINEL-HDR-XSESS",
		"SENTINEL-PCK", "SENTINEL-CONV", "SENTINEL-SID", "SENTINEL-CM"}
	hdr := map[string]string{"Thread-Id": sentinels[0], "Session-Id": sentinels[1], "X-Session-Id": sentinels[2]}
	bodyKeys := `"prompt_cache_key":"SENTINEL-PCK","conversation_id":"SENTINEL-CONV","session_id":"SENTINEL-SID","client_metadata":{"k":"SENTINEL-CM"}`
	respBody := `{"model":"gpt-4o","stream":true,` + bodyKeys + `,"input":[{"role":"user","content":"hi"}]}`
	wsFrame := `{"type":"response.create","model":"gpt-4o",` + bodyKeys + `,"input":[{"role":"user","content":"hi"}]}`
	chatBody := `{"model":"gpt-4o","stream":true,` + bodyKeys + `,"messages":[{"role":"user","content":"hi"}]}`
	messBody := `{"model":"gpt-4o","stream":true,"max_tokens":64,` + bodyKeys + `,"messages":[{"role":"user","content":"hi"}]}`

	lanes := []struct {
		name      string
		transport domain.CodexResponsesTransport
		run       func(t *testing.T, p *Proxy)
	}{
		{"http to http", domain.CodexResponsesTransportHTTP, func(t *testing.T, p *Proxy) {
			code, out := postConv(t, p, "ck-1", respBody, hdr)
			require.Equal(t, http.StatusOK, code, out)
		}},
		{"http to ws", domain.CodexResponsesTransportWS, func(t *testing.T, p *Proxy) {
			code, out := postConv(t, p, "ck-1", respBody, hdr)
			require.Equal(t, http.StatusOK, code, out)
		}},
		{"ws to ws", domain.CodexResponsesTransportHTTP, func(t *testing.T, p *Proxy) {
			h := http.Header{}
			for k, v := range hdr {
				h.Set(k, v)
			}
			wsConvSession(t, p, h, wsFrame)
		}},
		{"converted chat to resp", domain.CodexResponsesTransportHTTP, func(t *testing.T, p *Proxy) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
			req.Header.Set("Authorization", "Bearer ck-1")
			req.Header.Set("Content-Type", "application/json")
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			AIRouter(p).ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}},
		{"converted messages to resp", domain.CodexResponsesTransportHTTP, func(t *testing.T, p *Proxy) {
			rec := postAnthropicConv(t, p, messBody, hdr)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}},
		{"http body key only", domain.CodexResponsesTransportHTTP, func(t *testing.T, p *Proxy) {
			code, out := postConv(t, p, "ck-1", respBody, nil)
			require.Equal(t, http.StatusOK, code, out)
		}},
		{"ws body key only", domain.CodexResponsesTransportHTTP, func(t *testing.T, p *Proxy) {
			wsConvSession(t, p, nil, wsFrame)
		}},
	}
	for _, lane := range lanes {
		for _, withBook := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s book=%v", lane.name, withBook), func(t *testing.T) {
				var book *codexsdk.ConversationBook
				var mr *miniredis.Miniredis
				if withBook {
					mr = miniredis.RunT(t)
					book = newMiniredisBook(t, mr)
				}
				up, u := newConvUpstream(t)
				p := newConvTestProxy(t, up.URL, convTestOpts{
					accs: []convTestAcc{{id: 10}}, transport: lane.transport, book: book,
					pcs: []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp},
				})
				lane.run(t, p)
				require.Equal(t, 1, u.n())
				w := u.wire(t, 0)
				require.Equal(t, lane.transport == domain.CodexResponsesTransportWS || strings.HasPrefix(lane.name, "ws "), w.ws)
				if w.ws {
					require.NotEmpty(t, w.frames, "上游收到请求帧")
				} else {
					require.True(t, gjson.ValidBytes(w.body), "HTTP 体已解压")
				}
				wire := u.allBytes()
				require.Contains(t, wire, "Bearer "+convPAT(10), "扫描面含头")
				require.Contains(t, wire, `"client_metadata"`, "扫描面含解压后的体/帧")
				for _, s := range sentinels {
					require.NotContains(t, wire, s, "哨兵出网")
				}
				if withBook {
					require.Equal(t, int64(1), book.Stats().Opens, "带对话标识 → 会话路径（转换车道用原始头体）")
					flushBook(t, book)
					stored := miniredisContents(t, mr)
					require.Contains(t, stored, "codex:conv:", "扫描面含会话记录")
					require.Contains(t, stored, "codex:turn:", "扫描面含轮次记录")
					for _, s := range sentinels {
						require.NotContains(t, stored, s, "对话标识原文进 Redis")
					}
				}
			})
		}
	}
}

// miniredisContents 拼出 miniredis 全部键名与值（string 与 hash；其他类型直接
// 失败，防扫描面漏类型）。不用 Dump：它截断长值。
func miniredisContents(t *testing.T, mr *miniredis.Miniredis) string {
	t.Helper()
	var b strings.Builder
	for _, k := range mr.Keys() {
		b.WriteString(k + "\n")
		switch typ := mr.Type(k); typ {
		case "string":
			v, err := mr.Get(k)
			require.NoError(t, err)
			b.WriteString(v + "\n")
		case "hash":
			fields, err := mr.HKeys(k)
			require.NoError(t, err)
			for _, f := range fields {
				b.WriteString(f + "=" + mr.HGet(k, f) + "\n")
			}
		default:
			require.FailNowf(t, "未覆盖的 Redis 类型", "key=%s type=%s", k, typ)
		}
	}
	return b.String()
}
