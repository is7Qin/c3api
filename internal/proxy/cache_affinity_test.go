// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/scheduler"
)

func TestAffinityIdentity_prefersExplicitKeys(t *testing.T) {
	tests := []struct {
		name   string
		values [][]byte
		want   string
		has    bool
	}{
		{name: "prompt cache key", values: [][]byte{[]byte(`"prompt"`), []byte(`"conversation"`), []byte(`"session"`)}, want: "prompt", has: true},
		{name: "conversation fallback", values: [][]byte{nil, []byte(`"conversation"`), []byte(`"session"`)}, want: "conversation", has: true},
		{name: "session fallback", values: [][]byte{nil, nil, []byte(`"session"`)}, want: "session", has: true},
		{name: "null and missing", values: [][]byte{nil, []byte("null"), nil}, has: false},
		{name: "non-string ignored", values: [][]byte{[]byte("42"), []byte("true"), []byte(`[]`)}, has: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hash, has := affinityIdentity(tc.values[0], tc.values[1], tc.values[2])
			require.Equal(t, tc.has, has)
			if tc.has {
				require.Equal(t, scheduler.CacheAffinityHash(tc.want), hash)
			}
		})
	}
}

func TestAffinityIdentity_emptyExplicitKeyUsesNoAffinity(t *testing.T) {
	hash, has := affinityIdentity([]byte(`""`), []byte(`""`), []byte(`""`))
	require.Zero(t, hash)
	require.False(t, has)
}

func TestAffinityIdentityFromFrame_readsOnlyTopLevelExplicitKeys(t *testing.T) {
	frame := []byte(`{"input":{"session_id":"nested"},"conversation_id":"conversation"}`)
	hash, has := affinityIdentityFromFrame(frame)
	require.True(t, has)
	require.Equal(t, scheduler.CacheAffinityHash("conversation"), hash)
}
