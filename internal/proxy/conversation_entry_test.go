// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// rewindBody 可复用的请求体（AllocsPerRun 内 Seek 回零，不新分配）。
type rewindBody struct{ *bytes.Reader }

func (rewindBody) Close() error { return nil }

// entryRequest 构造一个带指定头与体的 POST 请求。
func entryRequest(body string, hdr map[string]string) (*http.Request, *bytes.Reader) {
	rd := bytes.NewReader([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r.Body = rewindBody{rd}
	return r, rd
}

// parseEntry 跑 HTTP 入口的读体与首遍扫描，返回写好的 reqMeta。
func parseEntry(t *testing.T, body string, hdr map[string]string) *reqMeta {
	t.Helper()
	p := &Proxy{cfg: Config{MaxBodySize: 1 << 20}}
	r, _ := entryRequest(body, hdr)
	rm := &reqMeta{}
	_, _, _, ok := p.readAndParseFormatBody(domain.FormatOpenAIResponses, httptest.NewRecorder(), r, rm, "rid", 1, time.Time{})
	require.True(t, ok)
	return rm
}

// wsEntry 跑 WS 入口（升级头 + 首帧）的取值。
func wsEntry(frame string, hdr map[string]string) *reqMeta {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	rm := &reqMeta{}
	rm.setWSEntryIdentity(h, []byte(frame))
	return rm
}

// C3-1：六个来源的优先级（头 Thread-Id > Session-Id > X-Session-Id > 体
// prompt_cache_key > conversation_id > session_id），HTTP 与 WS 同一规则；空值
// 与非字符串跳过；全缺 → hasConv=false。
func TestConversationIdentifierPrecedence(t *testing.T) {
	allBody := `{"model":"m","session_id":"b-sid","conversation_id":"b-conv","prompt_cache_key":"b-pck"}`
	cases := []struct {
		name string
		hdr  map[string]string
		body string
		want string
		has  bool
	}{
		{"thread header first", map[string]string{"Thread-Id": "h-thread", "Session-Id": "h-sess", "X-Session-Id": "h-xsess"}, allBody, "h-thread", true},
		{"session header second", map[string]string{"Session-Id": "h-sess", "X-Session-Id": "h-xsess"}, allBody, "h-sess", true},
		{"x-session header third", map[string]string{"X-Session-Id": "h-xsess"}, allBody, "h-xsess", true},
		{"lower-case header key normalised by Set", map[string]string{"thread-id": "h-thread"}, allBody, "h-thread", true},
		{"empty header skipped", map[string]string{"Thread-Id": ""}, allBody, "b-pck", true},
		{"prompt_cache_key", nil, allBody, "b-pck", true},
		{"conversation_id", nil, `{"model":"m","session_id":"b-sid","conversation_id":"b-conv"}`, "b-conv", true},
		{"session_id", nil, `{"model":"m","session_id":"b-sid"}`, "b-sid", true},
		{"empty and non-string skipped", nil, `{"model":"m","prompt_cache_key":"","conversation_id":42,"session_id":"b-sid"}`, "b-sid", true},
		{"unicode escape decoded on materialise", nil, `{"model":"m","prompt_cache_key":"a\u0062c"}`, "abc", true},
		{"nested keys ignored", nil, `{"model":"m","input":{"prompt_cache_key":"x"}}`, "", false},
		{"none", nil, `{"model":"m"}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for lane, rm := range map[string]*reqMeta{
				"http": parseEntry(t, tc.body, tc.hdr),
				"ws":   wsEntry(tc.body, tc.hdr),
			} {
				require.Equal(t, tc.has, rm.hasConv, lane)
				require.Equal(t, tc.want, rm.conversation(), lane)
			}
		})
	}
	// 体来源只记录原始切片（引用请求体，不物化）。
	rm := parseEntry(t, `{"model":"m","conversation_id":"b-conv"}`, nil)
	require.Empty(t, rm.convHeader)
	require.Equal(t, `"b-conv"`, string(rm.convBody))
}

// C3-2：粘性键 Session-Id 头 > X-Session-Id 头 > 体键；Thread-Id 不参与；无头时
// 哈希与改动前（只看体键）相同。HTTP 与 WS 一致。
func TestAffinityPrefersSessionHeaders(t *testing.T) {
	body := `{"model":"m","prompt_cache_key":"b-pck","conversation_id":"b-conv"}`
	cases := []struct {
		name string
		hdr  map[string]string
		want string
	}{
		{"session header", map[string]string{"Session-Id": "h-sess", "X-Session-Id": "h-xsess"}, "h-sess"},
		{"x-session header", map[string]string{"X-Session-Id": "h-xsess"}, "h-xsess"},
		{"thread header does not stick", map[string]string{"Thread-Id": "h-thread"}, "b-pck"},
		{"no header = body (unchanged)", nil, "b-pck"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for lane, rm := range map[string]*reqMeta{
				"http": parseEntry(t, body, tc.hdr),
				"ws":   wsEntry(body, tc.hdr),
			} {
				require.True(t, rm.HasAffinity, lane)
				require.Equal(t, scheduler.CacheAffinityHash(tc.want), rm.AffinityHash, lane)
			}
		})
	}
	// 无头、无体键：无粘性。
	require.False(t, parseEntry(t, `{"model":"m"}`, nil).HasAffinity)
	require.False(t, wsEntry(`{"model":"m"}`, nil).HasAffinity)
}

// C3-15 基线：改动前在 HEAD c5e4bce 上实测（计费关，readAndParseFormatBody 含
// MaxBytesReader + ReadAll 的固定开销；WS 入口当时只有 affinityIdentityFromFrame，
// 首帧带体键时 gjson.GetBytes 复制一次 Raw）。改动后带与不带对话标识都不得超过。
const (
	baseHTTPEntryAllocs      = 3
	baseWSEntryAllocsNoKey   = 0
	baseWSEntryAllocsBodyKey = 1
)

// C3-15：入口零新增分配（HTTP 与 WS，带/不带对话标识，头/体两种来源）。预留路径的
// 零分配由 scheduler 的 TestClaimAndIdentityZeroAlloc 钉住。
func TestFormatEntryConversationNoNewAllocs(t *testing.T) {
	p := &Proxy{cfg: Config{MaxBodySize: 1 << 20}}
	cases := []struct {
		name    string
		body    string
		hdr     map[string]string
		conv    bool
		wsLimit float64
	}{
		{"none", `{"model":"gpt-5","stream":true,"input":"hi"}`, nil, false, baseWSEntryAllocsNoKey},
		{"body key", `{"model":"gpt-5","stream":true,"prompt_cache_key":"pck-1","input":"hi"}`, nil, true, baseWSEntryAllocsBodyKey},
		{"headers", `{"model":"gpt-5","stream":true,"input":"hi"}`, map[string]string{"Thread-Id": "th-1", "Session-Id": "s-1"}, true, baseWSEntryAllocsNoKey},
		{"headers and body key", `{"model":"gpt-5","stream":true,"prompt_cache_key":"pck-1","input":"hi"}`, map[string]string{"Session-Id": "s-1"}, true, baseWSEntryAllocsBodyKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rd := entryRequest(tc.body, tc.hdr)
			w := httptest.NewRecorder()
			rm := &reqMeta{}
			n := testing.AllocsPerRun(200, func() {
				_, _ = rd.Seek(0, 0)
				if _, _, _, ok := p.readAndParseFormatBody(domain.FormatOpenAIResponses, w, r, rm, "rid", 1, time.Time{}); !ok {
					panic("entry rejected")
				}
			})
			require.LessOrEqual(t, n, float64(baseHTTPEntryAllocs))
			require.Equal(t, tc.conv, rm.hasConv)

			frame := []byte(tc.body)
			wsRM := &reqMeta{}
			ws := testing.AllocsPerRun(200, func() { wsRM.setWSEntryIdentity(r.Header, frame) })
			require.LessOrEqual(t, ws, tc.wsLimit)
			require.Equal(t, tc.conv, wsRM.hasConv)
		})
	}
}
