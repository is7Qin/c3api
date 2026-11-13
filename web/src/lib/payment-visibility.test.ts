// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 支付网关开关（运行时设置 payment.enabled）的前端判定与接线。
//   - 纯函数（payment-visibility.ts）：开/关两态精确断言（数组逐项、开启态引用不变）。
//   - 接线 tripwire：与 supplier-visibility.test.ts 同款的源码形状断言（本项目没有
//     jsdom / 渲染测试），只守最容易静默回归的形状：菜单两组都过滤、面包屑守卫先于
//     路径分支、三张页面都在闸门之内且管理面两页在 RequireAdmin 之内、闸门分支顺序、
//     设置页保存总开关后让开关缓存失效。
//   - 支付账户新建/编辑弹窗保持视口居中（不再被覆盖成右侧整高抽屉）。
// 运行：`node --test`。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { PAYMENT_PATHS, isPaymentPath, paymentNavFor } from './payment-visibility.ts'

describe('isPaymentPath', () => {
  it('支付订单、支付账户、钱包三张页面为真', () => {
    assert.equal(isPaymentPath('/app/orders'), true)
    assert.equal(isPaymentPath('/app/payment-accounts'), true)
    assert.equal(isPaymentPath('/user/wallet'), true)
  })

  it('前缀相同但不是同一段的路径、两台根路径与其余页面为假', () => {
    assert.equal(isPaymentPath('/app/ordersx'), false)
    assert.equal(isPaymentPath('/app/payment-accounts-x'), false)
    assert.equal(isPaymentPath('/user/wallet-x'), false)
    assert.equal(isPaymentPath('/app'), false)
    assert.equal(isPaymentPath('/user'), false)
    assert.equal(isPaymentPath('/app/redemption-codes'), false)
    assert.equal(isPaymentPath('/user/redemptions'), false)
  })

  it('路径集合与路由字面量一致', () => {
    assert.deepEqual([...PAYMENT_PATHS], ['/app/orders', '/app/payment-accounts', '/user/wallet'])
  })
})

describe('paymentNavFor', () => {
  const nav = [
    { to: '/app/redemption-codes', key: 'a' },
    { to: '/app/orders', key: 'b' },
    { to: '/app/payment-accounts', key: 'c' },
    { to: '/app/pricing', key: 'd' },
    { to: '/user/wallet', key: 'e' },
  ] as const

  it('开启 ⇒ 原样返回同一引用', () => {
    assert.equal(paymentNavFor(nav, true), nav)
  })

  it('关闭 ⇒ 只去掉三张支付页面，其余保序，且不改入参', () => {
    assert.deepEqual(paymentNavFor(nav, false), [
      { to: '/app/redemption-codes', key: 'a' },
      { to: '/app/pricing', key: 'd' },
    ])
    assert.equal(nav.length, 5)
  })
})

// —— 接线 tripwire（源码形状）——

const srcRoot = new URL('../', import.meta.url)
const read = (rel: string) => readFileSync(new URL(rel, srcRoot), 'utf8')
const count = (src: string, re: RegExp) => (src.match(re) ?? []).length
const shell = read('components/app-shell.tsx')
const app = read('App.tsx')
// 路由结构断言只看嵌套与顺序，不看排版：空白归一后匹配。
const appFlat = app.replace(/\s+/g, ' ')
const featuresHook = read('lib/use-features.ts')
const settings = read('pages/settings.tsx')
const paymentAccounts = read('pages/payment-accounts.tsx')

describe('接线：AppShell（菜单与面包屑）', () => {
  it('同一次 useFeatures() 取两个开关；管理组与用户组都经 paymentNavFor', () => {
    assert.ok(shell.includes('const { supplier, payment } = useFeatures()'))
    assert.ok(shell.includes('paymentNavFor(adminNavFor(adminNav, supplier), payment)'))
    assert.ok(shell.includes('paymentNavFor(userNav, payment)'))
  })

  it('菜单项路径与 PAYMENT_PATHS 同一组字面量', () => {
    for (const p of PAYMENT_PATHS) assert.equal(count(shell, new RegExp(`to: '${p}'`, 'g')), 1, p)
  })

  it('面包屑传入支付开关，且在任何路径分支之前先挡支付页面', () => {
    assert.match(shell, /breadcrumbFor\(location\.pathname, supplier, payment\)/)
    const at = shell.indexOf('function breadcrumbFor(')
    assert.notEqual(at, -1, '找不到 breadcrumbFor')
    const body = shell.slice(at, shell.indexOf('\n}\n', at))
    const guard = 'if (!payment && isPaymentPath(pathname)) return null'
    const firstBranch = body.indexOf('if (pathname.startsWith')
    assert.ok(body.includes(guard), 'breadcrumbFor 缺少支付页面守卫')
    assert.notEqual(firstBranch, -1, '找不到 breadcrumbFor 的路径分支')
    assert.ok(body.indexOf(guard) < firstBranch, '守卫必须先于 /app、/user 分支')
  })
})

describe('接线：路由闸门（App.tsx）', () => {
  it('三张支付页面各挂一次，且不再留在 app / user 支内', () => {
    assert.equal(count(appFlat, /element: <PaymentFeatureGate \/>/g), 1)
    assert.equal(count(appFlat, /path: 'app\/orders', element: <Orders \/>/g), 1)
    assert.equal(count(appFlat, /path: 'app\/payment-accounts', element: <PaymentAccounts \/>/g), 1)
    assert.equal(count(appFlat, /path: 'user\/wallet', element: <UserWallet \/>/g), 1)
    assert.equal(count(appFlat, /element: <(Orders|PaymentAccounts|UserWallet) \/>/g), 3)
    assert.equal(count(appFlat, /path: '(orders|payment-accounts|wallet)'/g), 0, '旧的 app/user 支内子路由必须移除')
  })

  it('闸门在外：管理面两页在 RequireAdmin 之内，钱包在 RequireAdmin 之外', () => {
    const s = appFlat.indexOf('element: <PaymentFeatureGate />')
    const admin = appFlat.indexOf('element: <RequireAdmin />', s)
    const orders = appFlat.indexOf("path: 'app/orders'")
    const accounts = appFlat.indexOf("path: 'app/payment-accounts'")
    const wallet = appFlat.indexOf("path: 'user/wallet'")
    assert.ok(s !== -1 && s < admin && admin < orders && orders < accounts && accounts < wallet, '顺序：闸门 → RequireAdmin → 两页 → 钱包')
    assert.doesNotMatch(appFlat.slice(s, wallet), /path: '(app|user)'/, '三张页面必须都在同一闸门支内')
    assert.ok(appFlat.slice(accounts, wallet).includes('] }'), '钱包必须在 RequireAdmin 的 children 之外')
  })

  it('闸门分支顺序：未登录 → 加载中 → 关闭 404 → 放行', () => {
    const i = app.indexOf('function PaymentFeatureGate')
    assert.notEqual(i, -1, '找不到 PaymentFeatureGate')
    const j = app.indexOf('\nfunction ', i + 1)
    const body = app.slice(i, j === -1 ? undefined : j)
    assert.ok(body.includes('const { payment, resolved } = useFeatures()'), '闸门必须从 useFeatures() 解构 resolved，不得另造常量')
    const branches = body.slice(body.indexOf('useFeatures()'))
    const tokens = ['getToken', 'resolved', '<NotFound />', '<Outlet />']
    const pos = tokens.map((tk) => branches.indexOf(tk))
    pos.forEach((p, k) => assert.notEqual(p, -1, `闸门缺少 ${tokens[k]}`))
    for (let k = 1; k < pos.length; k++) {
      assert.ok(pos[k] > pos[k - 1], `${tokens[k - 1]} 必须先于 ${tokens[k]}`)
    }
  })
})

describe('接线：开关取值与失效', () => {
  it('hook：payment fail-closed，查询键只有一处字面量', () => {
    assert.ok(featuresHook.includes('payment: q.data?.payment === true'), '加载中/失败必须按关闭（fail-closed）')
    assert.ok(featuresHook.includes("export const FEATURES_QUERY_KEY = ['user', 'features'] as const"))
    assert.ok(featuresHook.includes('queryKey: FEATURES_QUERY_KEY'))
  })

  it('设置页：只在保存总开关后让开关缓存失效（键与 hook 同源）', () => {
    assert.ok(settings.includes("const PAYMENT_SWITCH_KEY = 'payment.enabled'"))
    assert.ok(settings.includes('if (key === PAYMENT_SWITCH_KEY) qc.invalidateQueries({ queryKey: FEATURES_QUERY_KEY })'))
    assert.equal(count(settings, /FEATURES_QUERY_KEY/g), 2, '一处 import + 一处失效')
  })
})

describe('支付账户新建/编辑弹窗', () => {
  it('视口居中：不再覆盖默认居中定位为右侧整高抽屉', () => {
    const tags = paymentAccounts.match(/<DialogContent className="[^"]*"/g) ?? []
    assert.ok(tags.length > 0, '找不到 DialogContent')
    for (const tag of tags) assert.doesNotMatch(tag, /\b(left-auto|right-0|top-0|translate-[xy]-0|h-dvh)\b/, tag)
  })
})
