// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 真实消费链回归（spec 2026-08-16 sserelay-lines #7）：>8KB 单行
// response.completed 帧经 sserelay.Relay 全量到达 Observer → caller_responses.go
// 的 EventName 判定 + responsesCompletedUsage 提取成功。落 proxy 侧：
// responsesCompletedUsage 未导出，且 sserelay 内部测试包 import proxy 成环。
// usage 字段以 output 数组垫至 8192B 截断区外——旧实现 Data 截断于首 chunk
// → usage 提取落空 → 0 token 计费，本测试钉死该回归。

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/sserelay"
)

func TestSserelayCompletedUsageLongLine(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"response.completed","response":{"id":"r","model":"m","output":[`)
	item := `{"type":"function_call","name":"f","arguments":"aaaa"}`
	for i := 0; i < 400; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(item)
	}
	sb.WriteString(`],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}}}}`)
	payload := sb.String()
	require.Greater(t, len(payload), 8192, "usage 必须位于 8192B 截断区外（output 垫长）")
	src := "data: " + payload + "\n\n"
	var it, ot, tt, cr int64
	rec := httptest.NewRecorder()
	require.NoError(t, sserelay.Relay(context.Background(), rec, strings.NewReader(src), sserelay.Config{
		Observer: func(ev sserelay.Event) {
			// 镜像 caller_responses.go:89-92 消费链：EventName 判定 +
			// responsesCompletedUsage 提取
			if string(ev.EventName()) == "response.completed" {
				if u, ok := responsesCompletedUsage(ev.Data); ok {
					it, ot, tt, cr = u.it, u.ot, u.tt, u.cr
				}
			}
		},
	}))
	require.Equal(t, int64(10), it, "input_tokens 提取成功（长行 Data 全量 → usage 不落空 → 计费不归零）")
	require.Equal(t, int64(20), ot)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "input_tokens_details.cached_tokens")
	require.Equal(t, src, rec.Body.String(), "长行字节必须完整原样转发")
}
