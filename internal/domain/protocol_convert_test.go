// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"os"
	"strings"
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
		{"mess_to_chat", ProtocolConvertMessToChat, FormatAnthropic, FormatOpenAIChat},
		{"resp_to_chat", ProtocolConvertRespToChat, FormatOpenAIResponses, FormatOpenAIChat},
		{"off", ProtocolConvertOff, "", ""},
		{"unknown", ProtocolConvert("bogus"), "", ""},
		{"empty", ProtocolConvert(""), "", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.client, tc.dir.Client(), "Client(%q) [%s]", tc.dir, tc.name)
		require.Equal(t, tc.target, tc.dir.Target(), "Target(%q) [%s]", tc.dir, tc.name)
	}
}

// TestAllProtocolConverts 枚举完整性：AllProtocolConverts 必须恰好覆盖
// Valid() 的全部取值且两两不同——新增方向忘了登记，后续所有"穷举方向"的
// 门禁（convCallers / Capability / openapi 同源）都会静默漏掉它。
func TestAllProtocolConverts(t *testing.T) {
	all := AllProtocolConverts()
	require.Len(t, all, 6, "方向全集 = 4 既有 + 2 新增")
	seen := make(map[ProtocolConvert]struct{}, len(all))
	for _, pc := range all {
		require.True(t, pc.Valid(), "AllProtocolConverts 含非法方向 %q", pc)
		require.NotEqual(t, ProtocolConvertOff, pc, "off 不进方向全集")
		require.False(t, pc.Client() == "" || pc.Target() == "", "方向 %q 缺客户端/目标格式映射", pc)
		_, dup := seen[pc]
		require.False(t, dup, "方向全集重复：%q", pc)
		seen[pc] = struct{}{}
	}
}

// TestProtocolConvertEnumMatchesOpenAPI 枚举同源（spec §8(n)）：openapi.yaml 的
// GroupProtocolConvert enum 必须与 AllProtocolConverts() 逐值一致。漏改一边会
// 让 API 文档/前端类型与网关行为脱节（前端下拉里能选到网关不认的方向，或反之）。
func TestProtocolConvertEnumMatchesOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("../../openapi/openapi.yaml")
	require.NoError(t, err, "读取 openapi.yaml（测试工作目录 = internal/domain）")
	src := string(raw)
	const marker = "GroupProtocolConvert:"
	i := strings.Index(src, marker)
	require.Greater(t, i, 0, "openapi.yaml 缺 GroupProtocolConvert schema")
	rest := src[i:]
	j := strings.Index(rest, "enum: [")
	require.Greater(t, j, 0, "GroupProtocolConvert 缺 enum 行")
	rest = rest[j+len("enum: ["):]
	k := strings.Index(rest, "]")
	require.Greater(t, k, 0)
	var fromDoc []ProtocolConvert
	for _, part := range strings.Split(rest[:k], ",") {
		v := strings.TrimSpace(part)
		require.NotEmpty(t, v)
		fromDoc = append(fromDoc, ProtocolConvert(v))
	}
	require.ElementsMatch(t, AllProtocolConverts(), fromDoc,
		"openapi enum 与 domain.AllProtocolConverts() 必须同源（改一边就要改另一边）")
}
