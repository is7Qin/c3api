// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package codexproto_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/codexproto"
)

// 实现方（internal/sdkbridge）与消费方（internal/handler）之间的**双向可达性**：
// 这里同时扮演两边，证明 errors.As（认类型）与 errors.Is（认旧哨兵）是两条互不
// 依赖的判定——这正是"生产边从哨兵换成类型、老调用方与老测试零改动"能成立的根据。
var (
	errProducedByImpl = errors.New("codex: usage upstream error") // 模拟仍保留的旧哨兵
	errWrappedInside  = errors.New("network unreachable")         // 底层原因
)

// produced 复刻 sdkbridge 的产出形态：类型化错误 + 旧哨兵 + 底层原因，三层 %w。
func produced(kind codexproto.ErrorKind, cause error) error {
	return fmt.Errorf("%w: %w", &codexproto.Error{Kind: kind, Err: cause}, errProducedByImpl)
}

// TestErrorKindReachableViaAs 消费方只认 codexproto 的类型就能拿到分类。
func TestErrorKindReachableViaAs(t *testing.T) {
	for _, kind := range []codexproto.ErrorKind{codexproto.ErrKindAuthExpired, codexproto.ErrKindUpstream} {
		var ce *codexproto.Error
		require.True(t, errors.As(produced(kind, errWrappedInside), &ce), "errors.As 必须拿到 *codexproto.Error")
		require.Equal(t, kind, ce.Kind)
	}
}

// TestLegacySentinelReachableViaIs 旧哨兵在链里仍然可达——实现方自己的既有测试
// （大量 require.ErrorIs）零改动。
func TestLegacySentinelReachableViaIs(t *testing.T) {
	require.True(t, errors.Is(produced(codexproto.ErrKindUpstream, errWrappedInside), errProducedByImpl))
	// 底层原因也仍然可达（Unwrap 链完整）。
	require.True(t, errors.Is(produced(codexproto.ErrKindUpstream, errWrappedInside), errWrappedInside))
}

// TestErrorStringAndUnwrap 错误文案与链形状（诊断时靠它认来源）。
func TestErrorStringAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &codexproto.Error{Kind: codexproto.ErrKindAuthExpired, Err: cause}
	require.Equal(t, "codex usage: boom", e.Error())
	require.Equal(t, cause, e.Unwrap())
}

// TestUnknownErrorNotClassified 未知错误**不被误分类**：既不是 Error 类型、
// 也不命中任何已分类档。消费方的 default 分支据此保持"null/null + Warn，不误标"的
// 既有语义——这是分类改造最容易退化的一处。
func TestUnknownErrorNotClassified(t *testing.T) {
	for _, err := range []error{
		errors.New("some other failure"),
		errProducedByImpl, // 只有旧哨兵、没有类型化包装
		fmt.Errorf("wrapped: %w", errWrappedInside),
		fmt.Errorf("ctx: %w", errors.New("boom")),
	} {
		var ce *codexproto.Error
		require.False(t, errors.As(err, &ce), "未分类错误不得命中 *codexproto.Error：%v", err)
	}
}
