// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 必须是**外部**测试包（qualitymodel_test）：同包测试要验证 repository 的别名就得
// import repository，而 repository 又 import 本包 ⇒ import cycle not allowed in test。
package qualitymodel_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/qualitymodel"
	"github.com/is7qin/c3api/internal/repository"
)

// TestRoutingRowsAreSameTypeAsRepositoryAliases 钉住"同一个类型"而不是"两份同形
// struct"：可赋值性成立当且仅当 repository 的声明是别名（带 `=`）。
// （少了 `=` 的变异会在这里编译失败，而不是测试红——见 spec §5.2 / §5.5 R1。）
func TestRoutingRowsAreSameTypeAsRepositoryAliases(t *testing.T) {
	q := qualitymodel.RoutingQualityRow{Attempts: 7}
	var qAlias repository.RoutingQualityRow = q
	require.Equal(t, int64(7), qAlias.Attempts)

	f := qualitymodel.RoutingFlowRow{ChainCount: 3}
	var fAlias repository.RoutingFlowRow = f
	require.Equal(t, int64(3), fAlias.ChainCount)

	// 反向：从 repository 侧构造，赋回 canonical 类型。
	q2 := repository.RoutingQualityRow{QualityClassID: domain.QualityClassIDVal{}}
	var qBack qualitymodel.RoutingQualityRow = q2
	require.Equal(t, q2.QualityClassID, qBack.QualityClassID)
}

// TestRoutingSnapshotBeyondRetentionSentinelIsShared 钉住哨兵是**同一实例**。
//
// 为什么用 require.True(x == y) 而不是 require.Equal：Equal 对 error 走 ObjectsAreEqual
// → reflect.DeepEqual，两个**文案相同但各自 errors.New** 的实例会判为相等（恒真），
// 导致 M2 变异（repository 侧改成新 errors.New）无法被抓到。== 比较的是接口
// （类型指针, 数据指针）二元组，新实例必然不等。
func TestRoutingSnapshotBeyondRetentionSentinelIsShared(t *testing.T) {
	require.True(t, repository.ErrRoutingSnapshotBeyondRetention == qualitymodel.ErrRoutingSnapshotBeyondRetention,
		"repository 的哨兵必须是 qualitymodel 的同一值别名，不是同文案的新实例")

	// 文案是一字不改的契约（spec §4.2 / §3.1.1）。
	require.Equal(t, "routing flow snapshot beyond observation retention",
		qualitymodel.ErrRoutingSnapshotBeyondRetention.Error())
}

// TestRowsCarryNoRepositoryDependency 只是文档性的形状断言：两个行类型在无人
// 构造时也必须可零值使用（避免将来有人把它们改成带构造约束的类型）。
func TestRowsCarryNoRepositoryDependency(t *testing.T) {
	var q qualitymodel.RoutingQualityRow
	require.Zero(t, q.Attempts)
	require.True(t, q.BucketMinute.Equal(time.Time{}))

	var f qualitymodel.RoutingFlowRow
	require.Nil(t, f.PreviousAccountID)
	require.False(t, f.IsTerminal)
}
