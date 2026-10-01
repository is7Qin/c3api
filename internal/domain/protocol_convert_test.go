// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProtocolConvertDirectionMethods 钉住 X2 等价性证据：ProtocolConvert.Client()
// /Target() 对四方向与 off/未知方向的输出（与重构前 proxy 的转换方向表（已删除，
// 现由本方法取代）的客户端/模板两列逐分支一致；未知/off 方向回退零值 ""，仅防御）。
func TestProtocolConvertDirectionMethods(t *testing.T) {
	cases := []struct {
		name   string
		dir    ProtocolConvert
		client RequestFormat
		target RequestFormat
	}{
		{"chat_to_resp", ProtocolConvertChatToResp, FormatOpenAIChat, FormatOpenAIResponses},
		{"mess_to_resp", ProtocolConvertMessToResp, FormatAnthropic, FormatOpenAIResponses},
		{"resp_to_mess", ProtocolConvertRespToMess, FormatOpenAIResponses, FormatAnthropic},
		{"chat_to_mess", ProtocolConvertChatToMess, FormatOpenAIChat, FormatAnthropic},
		{"off", ProtocolConvertOff, "", ""},
		{"unknown", ProtocolConvert("bogus"), "", ""},
		{"empty", ProtocolConvert(""), "", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.client, tc.dir.Client(), "Client(%q) [%s]", tc.dir, tc.name)
		require.Equal(t, tc.target, tc.dir.Target(), "Target(%q) [%s]", tc.dir, tc.name)
	}
}
