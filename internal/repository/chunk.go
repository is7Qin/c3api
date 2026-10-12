// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// inChunkSize IN 分片上限。PostgreSQL 单条语句参数上限 65535（错误 54001
// "too many parameters"）——ent 的 IN 谓词（IDIn 等）把列表整体展开为参数，
// 实体数量（组/账号/用户/key）超过上限即崩溃（压测实证：847k 组 → ent
// eager-load 生成 `WHERE group_id IN (…847k…)` 超限；启动 fatal、运行中静默
// 失败返回空）。分片切块后单条语句只含一个块：最大参数 = 8192(IN) + 少量
// 其它谓词（如 DeactivateCodes 的 StatusNEQ），远低于 65535；ent 邻接跳是
// 独立语句（LoadKeys 的 m2o IN ≤ 块内 key 数）。
const inChunkSize = 8192

// chunkIDs 把 ids 按 ≤size 切成连续块（保序，块内保序；空输入返回零块）。
// 分片只按位置切，不做去重——去重是各调用点的语义决策（service 层批量入参
// 已去重；实体 ID 扫描天然无重复），分片不应改变输入语义。
// size <= 0 视为编程错误（调用点用常量，测试覆盖）。
func chunkIDs[T any](ids []T, size int) [][]T {
	if size <= 0 {
		panic("chunkIDs: size must be > 0")
	}
	if len(ids) == 0 {
		return nil
	}
	n := (len(ids) + size - 1) / size
	out := make([][]T, 0, n)
	for i := range n {
		lo := i * size
		hi := min(lo+size, len(ids))
		out = append(out, ids[lo:hi])
	}
	return out
}
