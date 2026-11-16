// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"fmt"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"
)

// 三条判据的接线本身（不是判据内容——码集归 SDK，判错判对由 SDK 自己的用例负责）。
// 这里钉的是网关最容易写错的三件事：包了一层的错误要能穿透、nil 不得当真、
// 别把"网络/取消"误当成"上游空闲超时"。
func TestFatalPredicates(t *testing.T) {
	// ClassifyFatal：判死帧 vs 内容帧。
	require.Error(t, ClassifyFatal([]byte(`{"type":"error","error":{"code":"token_invalidated"}}`)))
	require.NoError(t, ClassifyFatal([]byte(`{"type":"response.created","response":{"id":"r"}}`)))
	require.NoError(t, ClassifyFatal(nil))

	// IsInvalidBody：必须穿透 EnvelopeError 包装（SDK 在出网前拒绝 ⇒ not_sent）。
	wrapped := NewEnvelopeError(400, `{"error":{"message":"bad"}}`, codexsdk.ErrInvalidRequestBody)
	require.True(t, IsInvalidBody(wrapped), "包装后的本地拒绝必须仍被识别")
	require.True(t, IsInvalidBody(fmt.Errorf("dial: %w", codexsdk.ErrInvalidRequestBody)))
	require.False(t, IsInvalidBody(nil))
	require.False(t, IsInvalidBody(context.Canceled), "取消不是本地拒绝")

	// IsWSIdleTimeout：上游读空闲 ≠ 网关预算到期 ≠ 客户端取消。
	require.True(t, IsWSIdleTimeout(fmt.Errorf("recv: %w", codexsdk.ErrWSIdleTimeout)))
	require.False(t, IsWSIdleTimeout(nil))
	require.False(t, IsWSIdleTimeout(context.DeadlineExceeded))
}
