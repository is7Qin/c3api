// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package paymentregistry 是本二进制编译进来的全部支付协议。
//
// 这里是协议清单的**唯一**事实来源：新增协议 = 实现 domain.PaymentProvider +
// 在 Registry 里加一行；service 与 main 都不许 import 协议包（新增上游因此是
// 一处改动，而不是散落在装配面的多份字面量）。
//
// 协议实现**永不删除**：下线只把账户 enabled=false。删实现会让该账户的历史订单
// 永久无法 sync、迟到回调无法验签（v1 F48 的资金级结论）。
package paymentregistry

import (
	"fmt"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service/epay"
)

// Registry 构造本二进制的协议注册表；协议清单只此一份，main 不再拼字面量。
//
// 装配失败（nil 元素/空名/重名）原样回错、由 main 启动期炸掉——调用方拿不到
// 半成品注册表，也就不会把"少一个协议"带进运行期。
func Registry(hc *http.Client) (*domain.ProviderRegistry, error) {
	reg, err := domain.NewProviderRegistry([]domain.PaymentProvider{epay.New(hc)})
	if err != nil {
		return nil, fmt.Errorf("paymentregistry: %w", err)
	}
	return reg, nil
}
