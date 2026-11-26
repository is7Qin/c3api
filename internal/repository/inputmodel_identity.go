// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"errors"

	"github.com/is7qin/c3api/internal/inputmodel"
	"github.com/is7qin/c3api/internal/paymenterr"
)

// 输入契约（R1-c）与支付业务哨兵的**编译期同一性 + 同值断言**。
//
// 各处声明本身分散在各自的源文件里（贴近使用点，别名与它替换掉的定义同址），
// 这里只集中放断言：函数体每次 go build 都会做类型检查，所以去掉任何一个 `=`
// 都会立刻让构建失败——只写在 _test.go 里会被"测试没跑到"放过。
// 范式照搬 R1-b 的 assertRowIdentity（见 routing.go）。

// assertInputContractIdentity 钉住"同一个类型"而不是"两份同形 struct"：双向
// 可赋值成立当且仅当上面的声明是别名（带 `=`）。
func assertInputContractIdentity() {
	var a inputmodel.ListQuery
	var A ListQuery
	a = A
	A = a
	var b inputmodel.UsageQuery
	var B UsageQuery
	b = B
	B = b
	var c inputmodel.OrderQuery
	var C OrderQuery
	c = C
	C = c
	var d inputmodel.ErrLogQuery
	var D ErrLogQuery
	d = D
	D = d
	var e inputmodel.GroupPatch
	var E GroupPatch
	e = E
	E = e
	var f inputmodel.TemplatePatch
	var F TemplatePatch
	f = F
	F = f
	var g inputmodel.AccountPatch
	var G AccountPatch
	g = G
	G = g
	var h inputmodel.UserPatch
	var H UserPatch
	h = H
	H = h
	var i inputmodel.PriceEntryManual
	var I PriceEntryManual
	i = I
	I = i

	_ = []any{a, A}
}

// assertInputContractIdentity 是否被调用不影响断言生效：**函数体本身**每次 go build
// 都会做类型检查。`var _` 只是让它不被当成死代码删掉（同 routing.go 的先例）。
var _ = assertInputContractIdentity

// assertMerchantRefImmutable 钉住哨兵是**同一实例**且 `%w` 链未断。
//
// 为什么用 `==` 而不是 errors.Is / reflect.DeepEqual：本包对外承诺的是"同一个
// error 值"，只有 `==`（比较接口的类型指针 + 数据指针）能证明这一点；两枚文案
// 相同但各自 fmt.Errorf 的实例会让 DeepEqual 判等、让包外 `==` 静默失效。
// 为什么额外要 errors.Is 到 storeerr：业务哨兵仍必须是**冲突的细分**，
// 否则 mapRepoErr 那一层会把 400 覆盖面漏掉。
func assertMerchantRefImmutable() {
	if ErrMerchantRefImmutable != paymenterr.ErrMerchantRefImmutable {
		panic("repository.ErrMerchantRefImmutable 必须与 paymenterr 的同值是同一实例，不是同文案的新值")
	}
	if !errors.Is(ErrMerchantRefImmutable, ErrConflict) {
		panic("ErrMerchantRefImmutable 的 %w 链断了：errors.Is(_, ErrConflict) 必须成立")
	}
}

// 同样只是让断言不被当成死代码删掉。
var _ = assertMerchantRefImmutable
