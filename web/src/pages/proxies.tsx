// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面出口代理池（/app/proxies）。页面实现与供应商面共用（ProxyPoolPage），
// 面别只由作用域决定：本路由在 /app 下 ⇒ 默认作用域 = adminApi（/api/admin/proxies）。
// 因此管理面还多出 owner_uid 列与「平台/私池」删除文案分流。
import ProxyPoolPage from '@/components/proxy-pool-page'

export default function Proxies() {
  return <ProxyPoolPage />
}
