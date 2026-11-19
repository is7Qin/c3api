// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// attemptSamplingInserter 只为构造一个真实的 *usage.AttemptLogWorker（不启动、不投递）。
type attemptSamplingInserter struct{}

func (attemptSamplingInserter) InsertBatch(context.Context, []*domain.AttemptLog) error { return nil }

// TestAttemptDropCounterSeam 装配处的采样接缝是**函数值**，所以这里能直接钉住两件事：
//   - nil worker ⇒ nil 函数值（= 未装配 ⇒ 响应省略 sampling），绝不会出现「非 nil
//     函数值 + nil 接收者」这种装配成功、请求时 panic 的中间态；
//   - 真 worker ⇒ 可直接调用且读到 0（没有丢弃）。
//
// 不碰 PG：worker 只用内存队列构造，测试里既不 Start 也不 Enqueue。
func TestAttemptDropCounterSeam(t *testing.T) {
	require.Nil(t, attemptDropCounter(nil), "nil worker ⇒ nil 函数值（未装配）")

	worker, err := usage.NewAttemptLogWorker(usage.AttemptLogConfig{}, attemptSamplingInserter{}, nil)
	require.NoError(t, err)

	read := attemptDropCounter(worker)
	require.NotNil(t, read, "真 worker ⇒ 可读函数值")
	require.EqualValues(t, 0, read(), "新建 worker 还没丢过行")
}
