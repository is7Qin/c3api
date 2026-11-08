// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestRespChatRejectRowsCovered polReject 行与"有状态字段黑名单"必须一一对应：
// 内容模型里声明"不可静默丢弃"的成员，其字段必须真在 rejectRespStateful 的
// 列表里；反过来列表里的每个字段也必须有行。两边任一侧单独改动都能红。
func TestRespChatRejectRowsCovered(t *testing.T) {
	var rows []string
	for _, rw := range rowsOf(dirRespToChatReq) {
		if rw.pol == polReject {
			rows = append(rows, rw.name)
		}
	}
	require.Equal(t, []string{"previous_response_id"}, rows, "polReject 行清单变了")
	require.Contains(t, respChatStatefulFields, rows[0], "polReject 行动了但黑名单没动")
	// 黑名单里其余三个字段行为相同（同族、同错误），必须都真被拒。
	for _, f := range respChatStatefulFields {
		body := `{"input":"hi","` + f + `":` + statefulProbeValue(f) + `}`
		_, err := ConvertRequest([]byte(body), domain.ProtocolConvertRespToChat)
		require.ErrorIs(t, err, ErrStatefulFields, "有状态字段 %q 必须整请求拒绝", f)
		require.Contains(t, err.Error(), f, "错误信息必须点名该字段")
	}
}

// statefulProbeValue 各字段的最小合法形态（只为触发"存在且非 null"）。
func statefulProbeValue(field string) string {
	switch field {
	case "conversation":
		return `"conv_1"`
	case "prompt":
		return `{"id":"pmpt_1"}`
	case "context_management":
		return `[]`
	}
	return `"resp_1"`
}

// TestRespChatStatefulRejectedShape 拒绝必须是**可识别的 sentinel**（proxy 据此
// 回 400 + invalid_request_error，而不是并进"请求体非法"）。
func TestRespChatStatefulRejectedShape(t *testing.T) {
	_, err := ConvertRequest([]byte(`{"previous_response_id":"r1","conversation":"c1"}`), domain.ProtocolConvertRespToChat)
	require.ErrorIs(t, err, ErrStatefulFields)
	require.Contains(t, err.Error(), "previous_response_id")
	require.Contains(t, err.Error(), "conversation")
	// 非有状态的 normal body 不得走到这里。
	_, err = ConvertRequest([]byte(`{"input":"hi"}`), domain.ProtocolConvertRespToChat)
	require.NoError(t, err)
}
