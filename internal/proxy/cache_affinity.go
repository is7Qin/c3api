// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/scheduler"
)

var requestAffinityKeys = [][]byte{
	[]byte("stream"),
	[]byte("model"),
	[]byte("service_tier"),
	[]byte("prompt_cache_key"),
	[]byte("conversation_id"),
	[]byte("session_id"),
}

// 对话类请求头（规范形键）。入口直接查 r.Header map，不调 Get：Get 会对键做
// 规范化，而这三个常量本身已是规范形，直接索引零分配（设计 §2）。
const (
	hdrThreadID   = "Thread-Id"
	hdrSessionID  = "Session-Id"
	hdrXSessionID = "X-Session-Id"
)

// convHeaders 是入口三个对话类请求头的首值（缺失或空 = ""）。值直接引用
// r.Header 里的字符串，零复制。
type convHeaders struct {
	thread, session, xSession string
}

func readConvHeaders(h http.Header) convHeaders {
	return convHeaders{
		thread:   firstHeaderValue(h, hdrThreadID),
		session:  firstHeaderValue(h, hdrSessionID),
		xSession: firstHeaderValue(h, hdrXSessionID),
	}
}

func firstHeaderValue(h http.Header, canonicalKey string) string {
	if v := h[canonicalKey]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// sessionAffinity 头部粘性键：Session-Id 优先，其次 X-Session-Id（Thread-Id 不参与
// 粘性，保持与改动前的体键哈希一致）。
func (ch convHeaders) sessionAffinity() (uint64, bool) {
	if ch.session != "" {
		return scheduler.CacheAffinityHash(ch.session), true
	}
	if ch.xSession != "" {
		return scheduler.CacheAffinityHash(ch.xSession), true
	}
	return 0, false
}

// affinityIdentity 粘性键：Session-Id 头、X-Session-Id 头，然后体键
// prompt_cache_key > conversation_id > session_id（取第一个非空字符串）。无头时
// 哈希与改动前相同。
func affinityIdentity(ch convHeaders, rawPromptCacheKey, rawConversationID, rawSessionID []byte) (uint64, bool) {
	if hash, ok := ch.sessionAffinity(); ok {
		return hash, true
	}
	if hash, ok := affinityHashValue(rawPromptCacheKey); ok {
		return hash, true
	}
	if hash, ok := affinityHashValue(rawConversationID); ok {
		return hash, true
	}
	if hash, ok := affinityHashValue(rawSessionID); ok {
		return hash, true
	}
	return 0, false
}

func affinityHashValue(raw []byte) (uint64, bool) {
	value, ok := parseStringValue(raw)
	if !ok || len(value) == 0 {
		return 0, false
	}
	return scheduler.CacheAffinityHash(string(value)), true
}

var frameAffinityKeys = [...]string{"prompt_cache_key", "conversation_id", "session_id"}

// affinityIdentityFromFrame WS 版粘性键：升级头 Session-Id、X-Session-Id 优先；
// 否则按体键顺序取首帧第一个非空字符串（gjson 取值，哈希与改动前相同）。
// convRaw 是该体值在 frame 里的原始字节切片（含引号，引用 frame，零复制）——
// 体对话标识与体粘性键是同一组键、同一顺序、同一"非空字符串"判据，故同源。
// 头命中时不扫首帧，convRaw 为 nil（此时对话标识也来自头，见 setConversation）。
func affinityIdentityFromFrame(ch convHeaders, frame []byte) (hash uint64, ok bool, convRaw []byte) {
	if hash, ok := ch.sessionAffinity(); ok {
		return hash, true, nil
	}
	for _, key := range frameAffinityKeys {
		result := gjson.GetBytes(frame, key)
		if result.Type == gjson.String && result.Str != "" {
			return scheduler.CacheAffinityHash(result.Str), true, frameRawValue(frame, result, key)
		}
	}
	return 0, false, nil
}

// frameRawValue 取 gjson 结果在原 frame 中的原始字节区间（Index 已知时零扫描；
// 未知时回退 scanKeyValue 定位同一顶层键）。
func frameRawValue(frame []byte, r gjson.Result, key string) []byte {
	if r.Index > 0 && r.Index+len(r.Raw) <= len(frame) {
		return frame[r.Index : r.Index+len(r.Raw)]
	}
	if s, e, ok := scanKeyValue(frame, []byte(key)); ok {
		return frame[s:e]
	}
	return nil
}

// setWSEntryIdentity WS 入口：升级请求头 + 首帧 → 粘性键与对话标识。首帧体值
// 的原始切片由亲和取值同时给出（同一组键、同一顺序），零新增分配。
func (rm *reqMeta) setWSEntryIdentity(h http.Header, first []byte) {
	ch := readConvHeaders(h)
	var convRaw []byte
	rm.AffinityHash, rm.HasAffinity, convRaw = affinityIdentityFromFrame(ch, first)
	rm.setConversation(ch, convRaw, nil, nil)
}

// nonEmptyJSONString 原始值是非空 JSON 字符串（非 `""`、首字节是引号）。零分配。
func nonEmptyJSONString(raw []byte) bool {
	return len(raw) > 2 && raw[0] == '"'
}

// setConversation 按设计 §2 的六个来源取第一个非空：头 Thread-Id、Session-Id、
// X-Session-Id，然后体 prompt_cache_key、conversation_id、session_id（体值传入
// 前三者的原始切片）。只记录来源（头值或体原始切片），不物化字符串。
func (rm *reqMeta) setConversation(ch convHeaders, rawPromptCacheKey, rawConversationID, rawSessionID []byte) {
	rm.hasConv, rm.convHeader, rm.convBody = false, "", nil
	switch {
	case ch.thread != "":
		rm.convHeader = ch.thread
	case ch.session != "":
		rm.convHeader = ch.session
	case ch.xSession != "":
		rm.convHeader = ch.xSession
	default:
		switch {
		case nonEmptyJSONString(rawPromptCacheKey):
			rm.convBody = rawPromptCacheKey
		case nonEmptyJSONString(rawConversationID):
			rm.convBody = rawConversationID
		case nonEmptyJSONString(rawSessionID):
			rm.convBody = rawSessionID
		default:
			return
		}
	}
	rm.hasConv = true
}

// conversation 物化对话标识（只在 Codex 尝试构造 ConversationKey 时调用）：头值
// 直接用；体值经 parseStringValue 解码。
func (rm *reqMeta) conversation() string {
	if rm.convHeader != "" {
		return rm.convHeader
	}
	v, _ := parseStringValue(rm.convBody)
	return string(v)
}
