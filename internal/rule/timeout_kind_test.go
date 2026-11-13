// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- error_type=timeout 的规则面（spec-commit-state-and-round-billing §5） ---

// TestKindTimeoutAppendedAtEnd Kind 是 iota：新类别**必须**追加在末尾，否则
// 已落库/已序列化事件的整数语义会被改掉（streamFailureHealth 用 `f.Kind == 0`
// 判「不投递健康」）。
func TestKindTimeoutAppendedAtEnd(t *testing.T) {
	require.Equal(t, 0, int(KindOK))
	require.Equal(t, 1, int(Kind429))
	require.Equal(t, 2, int(Kind4xx))
	require.Equal(t, 3, int(Kind5xx))
	require.Equal(t, 4, int(KindNetwork), "既有类别编号不得漂移")
	require.Equal(t, 5, int(KindTimeout), "timeout 追加在末尾")

	require.Equal(t, "timeout", KindTimeout.String())
	require.Equal(t, KindTimeout, kindFromString("timeout"))
	require.Equal(t, "network", KindNetwork.String(), "既有 String() 未受影响")
	require.Equal(t, -1, int(kindFromString("timeouts")), "未知值仍不匹配")

	require.NoError(t, ValidateWhen(domain.RuleWhen{Kind: new("timeout")}), "when.kind 必须接受 timeout")
}

// TestSeedTimeoutRule seed-timeout（p18）语义：kind=timeout → throttle(account,
// open, 10m) + 502/"Upstream request failed"，**不带 http_status 条件**（已提交
// 的流内失败恒无状态码，带条件即永不命中——seed-5xx-503-overload 的历史教训）。
func TestSeedTimeoutRule(t *testing.T) {
	st := newFakeRuleStore()
	e := New(Config{}, st, nil, nil, nil)
	require.NoError(t, e.Reload(context.Background()))

	rules, err := st.ListRules(context.Background(), nil)
	require.NoError(t, err)
	var seed domain.Rule
	var found bool
	for i := range rules {
		if rules[i].Name == "seed-timeout" {
			seed, found = rules[i], true
			break
		}
	}
	require.True(t, found, "seed-timeout 必须存在（种子共 6 条）")
	require.Equal(t, 18, seed.Priority, "priority 18：排在 seed-5xx(20)/seed-network(25) 之前")
	require.True(t, seed.Enabled)
	require.NotNil(t, seed.When.Kind)
	require.Equal(t, "timeout", *seed.When.Kind)
	require.Nil(t, seed.When.HTTPStatus, "不带 http_status 条件（status_code=0 的已提交流永不命中）")
	require.Nil(t, seed.When.ErrorMessageContains, "不得带 error_message 条件（超时无上游文案）")

	require.NotNil(t, seed.Then.Throttle)
	require.Equal(t, domain.ThrottleScopeAccount, seed.Then.Throttle.Scope)
	require.Equal(t, domain.ThrottleModeOpen, seed.Then.Throttle.Mode)
	require.NotNil(t, seed.Then.Throttle.DurationMs)
	require.Equal(t, int64(10*60*1000), *seed.Then.Throttle.DurationMs, "10 分钟 open 摘除")
	require.NotNil(t, seed.Then.ResponseCode)
	require.Equal(t, 502, *seed.Then.ResponseCode)
	require.NotNil(t, seed.Then.CustomMessage)
	require.Equal(t, "Upstream request failed", *seed.Then.CustomMessage)

	// 分类面：timeout 事件首中 seed-timeout（10m），不与 5xx/network 串味。
	then, punish := e.Classify(Event{AccountID: 1, Model: "gpt-4o", Kind: KindTimeout})
	require.True(t, punish)
	require.NotNil(t, then.Throttle)
	require.Equal(t, int64(10*60*1000), *then.Throttle.DurationMs)
	require.Equal(t, 502, *then.ResponseCode)

	then5xx, _ := e.Classify(Event{AccountID: 1, Model: "gpt-4o", Kind: Kind5xx})
	require.Equal(t, int64(10*60*1000), *then5xx.Throttle.DurationMs, "5xx 仍走 seed-5xx")
	thenNet, _ := e.Classify(Event{AccountID: 1, Model: "gpt-4o", Kind: KindNetwork})
	require.Equal(t, int64(5000), *thenNet.Throttle.DurationMs, "network 仍走自己的 5s")

	// 种子幂等：重载不重复写。
	require.NoError(t, e.Reload(context.Background()))
	require.Equal(t, int64(6), mustCountAny(t, st), "种子总数 6 条")
}
