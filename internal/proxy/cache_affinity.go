// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
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

func affinityIdentity(rawPromptCacheKey, rawConversationID, rawSessionID []byte) (uint64, bool) {
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

func affinityIdentityFromFrame(frame []byte) (uint64, bool) {
	for _, key := range []string{"prompt_cache_key", "conversation_id", "session_id"} {
		result := gjson.GetBytes(frame, key)
		if result.Type == gjson.String && result.String() != "" {
			return scheduler.CacheAffinityHash(result.String()), true
		}
	}
	return 0, false
}
