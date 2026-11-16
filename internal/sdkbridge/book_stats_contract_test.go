// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"
)

// 统计口径漂移门禁。/ops/workers 的 conv_*/shape_* 键是对外契约（openapi.yaml 的
// WorkerStatus.stats description 与 web locales 的 ops.stats.* 都按它渲染）。SDK 增删
// 一个计数器和本包漏改一个字段，在运行时表现为"前端少一格/多一格"，没有别的地方会红。
//
// 所以断言是**集合相等**（双向）：多一个键、少一个键都失败，迫使改动者显式决定。
func TestBookStatsKeysMatchSDK(t *testing.T) {
	book := newTestBook(newChanConvStore())
	var n atomic.Int64
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{LocalRejects: n.Load})

	raw, err := json.Marshal(w.Stats())
	require.NoError(t, err)
	var got map[string]int64
	require.NoError(t, json.Unmarshal(raw, &got))

	want := map[string]bool{"conv_local_rejects": true}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[codexsdk.ConversationStats]()) {
		want["conv_"+snakeCase(f.Name)] = true
	}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[codexsdk.ShapeStats]()) {
		want["shape_"+snakeCase(f.Name)] = true
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	require.Equal(t, want, keys)

	// 本地拒绝计数来自注入的读口（计数所有权仍在 proxy 的转发面）。
	before := got["conv_local_rejects"]
	n.Add(1)
	raw, err = json.Marshal(w.Stats())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, before+1, got["conv_local_rejects"])
}

var snakeBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])|([A-Z]+)([A-Z][a-z])`)

// snakeCase Go 字段名 → json 蛇形键（WSUntrackedConnections → ws_untracked_connections）。
func snakeCase(s string) string {
	s = snakeBoundary.ReplaceAllString(s, "${1}${3}_${2}${4}")
	return strings.ToLower(s)
}
