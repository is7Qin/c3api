// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"sort"
)

// ProviderRegistry 本二进制里有哪些支付协议（spec 支付网关 v2 §4，N53–N55）。
//
// 为什么要有它：装配面此前是 main 里的一份 []PaymentProvider 字面量，"重名/空名"
// 由 service 自查、每次读取线性遍历——"取到哪一份"于是变成装配顺序的函数。注册表
// 把三条不变量收在一处：**元素非 nil、名字唯一、枚举定序**。
//
// 错误一律在启动期炸掉（由 main 在 service.New 之前调用），运行期不猜"用哪个
// 协议"。协议实现**永不删除**：下线只把账户 enabled=false，删实现会让该账户的
// 历史订单永久无法 sync、迟到回调无法验签（v1 F48 的资金级结论）。
type ProviderRegistry struct {
	byName map[ProviderName]PaymentProvider
	// sortedAll 构造期按 name 升序算好：**唯一**排序点。调用方不得再排序——
	// 多一处排序就是"两份顺序可能分叉"，管理面输出与测试断言都会不稳定。
	// 只在构造期写，之后只读，故无需锁。
	sortedAll []PaymentProvider
}

// NewProviderRegistry 校验并定序装配协议表：nil 元素 / 空 Name() / 重名 → error。
//
// 错误带上 name 或下标，装配失败时能直接定位是哪一行写错了（而不是"注册表坏了"）。
func NewProviderRegistry(providers []PaymentProvider) (*ProviderRegistry, error) {
	r := &ProviderRegistry{
		byName:    make(map[ProviderName]PaymentProvider, len(providers)),
		sortedAll: make([]PaymentProvider, 0, len(providers)),
	}
	for i, p := range providers {
		if p == nil {
			return nil, fmt.Errorf("domain: payment provider #%d is nil", i)
		}
		name := p.Name()
		if name == "" {
			return nil, fmt.Errorf("domain: payment provider #%d has empty name", i)
		}
		if _, dup := r.byName[name]; dup {
			return nil, fmt.Errorf("domain: duplicate payment provider %q", name)
		}
		r.byName[name] = p
		r.sortedAll = append(r.sortedAll, p)
	}
	sort.Slice(r.sortedAll, func(i, j int) bool { return r.sortedAll[i].Name() < r.sortedAll[j].Name() })
	return r, nil
}

// Get 按名字取协议实现（选路与回调都是"按名字取"）；未注册 → false。
//
// nil 接收者等价于空注册表：测试里的 Service 字面量可能不装协议，读路径不该因此
// panic（生产装配由 New 保证非 nil）。
func (r *ProviderRegistry) Get(name ProviderName) (PaymentProvider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byName[name]
	return p, ok
}

// All 返回 name 升序的全部协议。
//
// 返回副本而不是内部切片：调用方一旦就地排序/截断，唯一排序点的缓存就被改写了，
// 后续所有输出（管理面列表、测试断言）随之漂移——复制是这条不变量最便宜的保险。
//
// nil 接收者与**空注册表**返回同一个值 nil（spec v2 §4 / N61："nil ≡ 空注册表"）：
// "没有协议"只有一种表示，调用方不必区分"未装配"与"装配了 0 个"。需要空数组的
// 输出面（管理面元数据端点）自己 `make([]T, 0)`，语义由它承担。
func (r *ProviderRegistry) All() []PaymentProvider {
	if r == nil || len(r.sortedAll) == 0 {
		return nil
	}
	out := make([]PaymentProvider, len(r.sortedAll))
	copy(out, r.sortedAll)
	return out
}
