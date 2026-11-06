// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 供应商面出口代理池（/user/supplier/proxies）。**复用同一页面组件**：本路由挂在
// App.tsx 的 SupplierScope 之下 ⇒ 作用域 = supplierApi（base /api/user/supplier），
// 相对路径 '/proxies' 即落到 /api/user/supplier/proxies（spec §3.6：只看得到、只写得了
// owner_uid = 本人 的条目；平台代理既不可见也不可绑）。
import ProxyPoolPage from '@/components/proxy-pool-page'

export default function SupplierProxies() {
  return <ProxyPoolPage />
}
