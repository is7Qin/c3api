// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// sdkHTTPError 模拟 SDK 内部 HTTP 错误：刻意只用裸字段（StatusCode+Raw），不带
// StatusCode()/RawJSON() 方法——沿用"包装动机"的原始形状。注意 SDK 的 *HTTPError
// 现在**有** RawJSON()，但 StatusCode 仍是字段，方法面依旧不完整。
type sdkHTTPError struct {
	status int
	raw    string
}

func (e *sdkHTTPError) Error() string { return fmt.Sprintf("sdk http %d", e.status) }

// TestEnvelopeErrorProtocol StatusCode()/RawJSON() 协议（网关 statusOf/
// upstreamBody 零改动复用面）。
func TestEnvelopeErrorProtocol(t *testing.T) {
	e := NewEnvelopeError(429, `{"error":{"message":"rate limited"}}`, &sdkHTTPError{status: 429, raw: "x"})
	require.Equal(t, 429, e.StatusCode())
	require.Equal(t, `{"error":{"message":"rate limited"}}`, e.RawJSON())
	require.Equal(t, "sdk http 429", e.Error(), "Error 委托被包装 SDK 错误")
}

// TestEnvelopeErrorUnwrapChain Unwrap 保留 errors.As 链：经任意层包装仍能命中
// 底层 SDK 错误类型与网关协议（双源去重 / statusOf / upstreamErrMsg 的
// errors.As 穿透）。
func TestEnvelopeErrorUnwrapChain(t *testing.T) {
	inner := &sdkHTTPError{status: 401, raw: `{"error":{"message":"bad token"}}`}
	e := NewEnvelopeError(401, inner.raw, inner)
	wrapped := fmt.Errorf("codex call failed: %w", e)

	var target *sdkHTTPError
	require.True(t, errors.As(wrapped, &target), "errors.As 穿透信封命中 SDK 错误类别")
	require.Same(t, inner, target)

	type statusCoder interface{ StatusCode() int }
	var sc statusCoder
	require.True(t, errors.As(wrapped, &sc), "errors.As 命中 StatusCode() 协议")
	require.Equal(t, 401, sc.StatusCode())

	type rawJSONer interface{ RawJSON() string }
	var rj rawJSONer
	require.True(t, errors.As(wrapped, &rj), "errors.As 命中 RawJSON() 协议")
	require.Equal(t, `{"error":{"message":"bad token"}}`, rj.RawJSON())
}

// TestEnvelopeErrorNilErr 无底层 SDK 错误：Unwrap 返回 nil（errors.As 链自然
// 中断），Error 非空，信封协议仍可用。
func TestEnvelopeErrorNilErr(t *testing.T) {
	e := NewEnvelopeError(502, "oops", nil)
	require.Equal(t, 502, e.StatusCode())
	require.Equal(t, "oops", e.RawJSON())
	require.NotEmpty(t, e.Error())
	require.Nil(t, e.Unwrap())
	var target *sdkHTTPError
	require.False(t, errors.As(e, &target), "无 Err 时链中断——底层 SDK 类型不可达")
}
