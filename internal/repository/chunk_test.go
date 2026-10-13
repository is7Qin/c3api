// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChunkIDs 分片边界：整除 / 余数 / 0 / 1 / 保序 / 不去重 / size 非法。
func TestChunkIDs(t *testing.T) {
	t.Run("exact multiple", func(t *testing.T) {
		ids := make([]int64, 2*inChunkSize)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		chunks := chunkIDs(ids, inChunkSize)
		require.Len(t, chunks, 2)
		require.Len(t, chunks[0], inChunkSize)
		require.Len(t, chunks[1], inChunkSize)
		require.Equal(t, int64(1), chunks[0][0])
		require.Equal(t, int64(inChunkSize), chunks[0][inChunkSize-1])
		require.Equal(t, int64(inChunkSize+1), chunks[1][0])
		require.Equal(t, int64(2*inChunkSize), chunks[1][inChunkSize-1])
	})

	t.Run("remainder", func(t *testing.T) {
		ids := make([]int64, inChunkSize+1)
		for i := range ids {
			ids[i] = int64(i)
		}
		chunks := chunkIDs(ids, inChunkSize)
		require.Len(t, chunks, 2)
		require.Len(t, chunks[0], inChunkSize)
		require.Len(t, chunks[1], 1)
		require.Equal(t, ids[inChunkSize], chunks[1][0], "余数块保序")
	})

	t.Run("empty", func(t *testing.T) {
		require.Nil(t, chunkIDs([]int64{}, inChunkSize))
		require.Nil(t, chunkIDs[int64](nil, inChunkSize))
	})

	t.Run("single", func(t *testing.T) {
		chunks := chunkIDs([]int64{42}, inChunkSize)
		require.Len(t, chunks, 1)
		require.Equal(t, []int64{42}, chunks[0])
	})

	t.Run("preserves order and duplicates", func(t *testing.T) {
		// 跨块重复（重复值落在不同块）：分片不合并不去重，块间按输入顺序。
		ids := make([]int64, 0, inChunkSize+2)
		for i := range inChunkSize + 1 {
			ids = append(ids, int64(i))
		}
		ids = append(ids, 0) // 与首元素重复
		chunks := chunkIDs(ids, inChunkSize)
		require.Len(t, chunks, 2)
		require.Equal(t, ids[:inChunkSize], chunks[0], "块内容 = 输入连续切片（含重复）")
		require.Equal(t, ids[inChunkSize:], chunks[1])
	})

	t.Run("generic non-int", func(t *testing.T) {
		s := []string{"a", "b", "c"}
		chunks := chunkIDs(s, 2)
		require.Equal(t, [][]string{{"a", "b"}, {"c"}}, chunks)
	})

	t.Run("size zero panics", func(t *testing.T) {
		require.Panics(t, func() { chunkIDs([]int64{1}, 0) })
		require.Panics(t, func() { chunkIDs([]int64{1}, -1) })
	})

	t.Run("size larger than input", func(t *testing.T) {
		chunks := chunkIDs([]int64{1, 2, 3}, inChunkSize)
		require.Len(t, chunks, 1)
		require.Equal(t, []int64{1, 2, 3}, chunks[0])
	})
}
