// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOverloadSeedMatchesCommittedInBandFailures 是本次需求的正反门禁：
// 不合成状态码的 5xx（status=nil）只要文案含 overload 就命中过载规则、不惩罚
// 不归一；非过载文案仍落 seed-5xx（502 + 10 分钟冷却）。
func TestOverloadSeedMatchesCommittedInBandFailures(t *testing.T) {
	sink := newFakeSink(10)
	e, _ := newTestEngineWithSink(t, sink, nil) // 空表 → Reload 落种子

	// 已提交的流内失败：规范 §3.1 不合成状态码 ⇒ HTTPStatus 恒 nil。
	overload := Event{AccountID: 7, Kind: Kind5xx, ErrorMessage: "upstream overloaded, retry later", OccurredAt: at(0)}
	then, punish := e.Classify(overload)
	require.False(t, punish, "过载命中不得惩罚")
	require.Nil(t, then.Throttle)
	require.Nil(t, then.ResponseCode, "过载码原样透传")
	require.Nil(t, then.CustomMessage, "过载文原样透传")

	e.HandleEvent(context.Background(), overload)
	require.Zero(t, sink.countThrottle(), "过载命中不得节流账号")

	// 反向门禁：非过载 5xx 仍然归一 502 + open 10m。
	plain := Event{AccountID: 7, Kind: Kind5xx, ErrorMessage: "upstream exploded", OccurredAt: at(1)}
	then, punish = e.Classify(plain)
	require.True(t, punish)
	require.Equal(t, 502, *then.ResponseCode)
	require.Equal(t, "Upstream request failed", *then.CustomMessage)
	require.Equal(t, int64(10*60*1000), *then.Throttle.DurationMs)

	e.HandleEvent(context.Background(), plain)
	require.Equal(t, 1, sink.countThrottle())
	require.Equal(t, int64(10*60*1000), *sink.lastThrottle().DurationMs, "非过载仍罚 10 分钟")
}
