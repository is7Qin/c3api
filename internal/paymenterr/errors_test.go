// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 必须是**外部**测试包（paymenterr_test）：同包测试要验证 repository 的别名就得
// import repository，而 repository 又 import 本包 ⇒ import cycle not allowed in test。
package paymenterr_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/paymenterr"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/storeerr"
)

// TestMerchantRefImmutableAliasIsSameValue 钉住 repository 侧的别名是**同一实例**。
//
// 为什么用 require.True(x == y) 而不是 require.Equal：Equal 对 error 走
// ObjectsAreEqual → reflect.DeepEqual，两枚**文案相同但各自 errors.New /
// fmt.Errorf** 的实例会判为相等（恒真），于是"M2 变异（repository 侧改成新实例）"
// 抓不到。== 比较的是接口 (类型指针, 数据指针) 二元组，新实例必然不等。
// （范式同 R1-b 的 qualitymodel 侧断言，见 qualitymodel/rows_test.go。）
func TestMerchantRefImmutableAliasIsSameValue(t *testing.T) {
	require.True(t, repository.ErrMerchantRefImmutable == paymenterr.ErrMerchantRefImmutable,
		"repository 的哨兵必须是 paymenterr 的同一值别名，不是同文案的新实例")
}

// TestMerchantRefImmutableKeepsConflictChain 钉住 %w 派生链仍在 canonical 上。
//
// 为什么右侧必须是 storeerr.ErrConflict 而不是 repository.ErrConflict：后者本身是
// storeerr 的别名，自洽重建（"派生哨兵与它包裹的 conflict 一起换成新实例"）时
// 前者仍为 true，没有判别力。只有钉在 canonical 上，"派生链断了"才会暴露。
func TestMerchantRefImmutableKeepsConflictChain(t *testing.T) {
	require.True(t, errors.Is(paymenterr.ErrMerchantRefImmutable, storeerr.ErrConflict),
		"errors.Is(_, storeerr.ErrConflict) 必须成立：%w 派生链断开了")

	// 反向必须不成立：本哨兵是 ErrConflict 的**细分**，不是它的同义词。
	// 若这条变红，说明有人把它写成了 ErrConflict 本身，paymentadmin 的 400 覆盖
	// 会退化成对所有冲突都判 400（409 分支永远走不到）。
	require.False(t, errors.Is(storeerr.ErrConflict, paymenterr.ErrMerchantRefImmutable),
		"ErrConflict 不得被误认成 ErrMerchantRefImmutable")
}

// TestMerchantRefImmutableTextIsContract 钉住文案逐字不变（spec §3.2）。
//
// 文案里的 "repository: " 前缀来自 storeerr（那里刻意保留了这个历史前缀），
// 后缀是支付域自己的说明。改任何一个字节都是 API 契约变更（这枚错误的文案会
// 直接回显给管理台），所以逐字断言。
func TestMerchantRefImmutableTextIsContract(t *testing.T) {
	require.Equal(t, "repository: conflict: merchant_ref is read-only",
		paymenterr.ErrMerchantRefImmutable.Error())
}
