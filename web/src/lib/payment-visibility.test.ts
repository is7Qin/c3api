// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 支付网关开关（运行时设置 payment.enabled）的前端判定与接线。
//   - 纯函数（payment-visibility.ts）：集合精确等于两张管理面页面；开/关两态精确断言
//     （数组逐项、开启态引用不变）。
//   - 接线 tripwire：与 supplier-visibility.test.ts 同款的源码形状断言（本项目没有
//     jsdom / 渲染测试）。保护对象是「支付**管理面**」：管理面受闸、钱包无条件可达、
//     充值区块局部受闸；另外守住 `/user/redemptions` 只剩兼容重定向。
//   - 支付账户新建/编辑弹窗保持视口居中（不再被覆盖成右侧整高抽屉）。
// 运行：`node --test`。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { PAYMENT_PATHS, isPaymentPath, paymentNavFor } from './payment-visibility.ts'

describe('isPaymentPath', () => {
  it('支付管理面两页为真，用户面钱包与兼容兑换路由为假', () => {
    assert.equal(isPaymentPath('/app/orders'), true)
    assert.equal(isPaymentPath('/app/payment-accounts'), true)
    // 钱包现在是「余额 + 充值 + 兑换 + 两类记录」的用户入口：支付关闭时仍要可达，
    // 不能因为「含充值内容」被判成支付页（否则菜单/面包屑会在关闭时整页消失）。
    assert.equal(isPaymentPath('/user/wallet'), false)
    assert.equal(isPaymentPath('/user/redemptions'), false)
  })

  it('前缀相同但不是同一段的路径、两台根路径与其余页面为假', () => {
    assert.equal(isPaymentPath('/app/ordersx'), false)
    assert.equal(isPaymentPath('/app/payment-accounts-x'), false)
    assert.equal(isPaymentPath('/user/wallet-x'), false)
    assert.equal(isPaymentPath('/app'), false)
    assert.equal(isPaymentPath('/user'), false)
    assert.equal(isPaymentPath('/app/redemption-codes'), false)
  })

  it('段前缀（子路径）仍为真', () => {
    assert.equal(isPaymentPath('/app/orders/123'), true)
    assert.equal(isPaymentPath('/app/payment-accounts/123'), true)
  })

  it('路径集合与 spec 一致：只有两张管理面页面', () => {
    assert.deepEqual([...PAYMENT_PATHS], ['/app/orders', '/app/payment-accounts'])
    assert.equal(PAYMENT_PATHS.length, 2)
  })
})

describe('paymentNavFor', () => {
  // 用户组的钱包必须在关闭时保留，所以它不在过滤集合里。
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

  it('关闭 ⇒ 只去掉两张管理面页面，钱包保留，其余保序，且不改入参', () => {
    assert.deepEqual(paymentNavFor(nav, false), [
      { to: '/app/redemption-codes', key: 'a' },
      { to: '/app/pricing', key: 'd' },
      { to: '/user/wallet', key: 'e' },
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

// 从 marker 所在位置向前找它所在的对象/块的 '{'，再按花括号配平取出整块。
// 只用于本仓库手写的路由与 JSX（花括号天然配平），不做通用解析。
function blockOf(src: string, marker: string): string {
  const at = src.indexOf(marker)
  assert.notEqual(at, -1, `找不到标记：${marker}`)
  const open = src.lastIndexOf('{', at)
  assert.notEqual(open, -1, `标记之前没有 '{'：${marker}`)
  let depth = 0
  for (let i = open; i < src.length; i++) {
    const c = src[i]
    if (c === '{') depth++
    else if (c === '}') {
      depth--
      if (depth === 0) return src.slice(open, i + 1)
    }
  }
  return assert.fail(`花括号不平衡：${marker}`)
}

const gateBlock = blockOf(app, 'element: <PaymentFeatureGate />')
const userBlock = blockOf(app, "path: 'user',")

describe('接线：AppShell（菜单与面包屑）', () => {
  it('同一次 useFeatures() 取两个开关；管理组与用户组都经 paymentNavFor', () => {
    assert.ok(shell.includes('const { supplier, payment } = useFeatures()'))
    assert.ok(shell.includes('paymentNavFor(adminNavFor(adminNav, supplier), payment)'))
    assert.ok(shell.includes('paymentNavFor(userNav, payment)'))
  })

  it('菜单项路径与 PAYMENT_PATHS 同一组字面量', () => {
    for (const p of PAYMENT_PATHS) assert.equal(count(shell, new RegExp(`to: '${p}'`, 'g')), 1, p)
  })

  it('用户组只留钱包入口：/user/wallet 恰好 1 次，/user/redemptions 0 次', () => {
    assert.equal(count(shell, /to: '\/user\/wallet'/g), 1)
    assert.equal(count(shell, /\/user\/redemptions/g), 0, '旧兑换导航项必须删除')
    assert.equal(count(shell, /'user\.nav\.redemptions'/g), 0, '旧兑换导航文案键不得残留')
  })

  it('面包屑传入支付开关，且在任何路径分支之前先挡支付管理面', () => {
    assert.match(shell, /breadcrumbFor\(location\.pathname, supplier, payment\)/)
    const at = shell.indexOf('function breadcrumbFor(')
    assert.notEqual(at, -1, '找不到 breadcrumbFor')
    const body = shell.slice(at, shell.indexOf('\n}\n', at))
    const guard = 'if (!payment && isPaymentPath(pathname)) return null'
    const firstBranch = body.indexOf('if (pathname.startsWith')
    assert.ok(body.includes(guard), 'breadcrumbFor 缺少支付管理面守卫')
    assert.notEqual(firstBranch, -1, '找不到 breadcrumbFor 的路径分支')
    assert.ok(body.indexOf(guard) < firstBranch, '守卫必须先于 /app、/user 分支')
  })
})

describe('接线：路由（App.tsx）', () => {
  it('闸门恰好一次，且只包住两张管理面页面', () => {
    assert.equal(count(appFlat, /element: <PaymentFeatureGate \/>/g), 1)
    assert.equal(count(gateBlock, /path: 'app\/orders'/g), 1)
    assert.equal(count(gateBlock, /path: 'app\/payment-accounts'/g), 1)
    // 钱包不在闸门内：闸门里既没有它的路由，也没有它的组件。
    assert.equal(count(gateBlock, /path: 'wallet'/g), 0, '钱包路由不得留在闸门支内')
    assert.equal(count(gateBlock, /<UserWallet \/>/g), 0, 'UserWallet 不得是闸门的 children')
    // 两张管理面页面仍在 RequireAdmin 之内。
    const adminBlock = blockOf(gateBlock, 'element: <RequireAdmin />')
    assert.equal(count(adminBlock, /path: 'app\/orders'/g), 1)
    assert.equal(count(adminBlock, /path: 'app\/payment-accounts'/g), 1)
    assert.equal(count(gateBlock, /element: <(Orders|PaymentAccounts) \/>/g), 2)
  })

  it('钱包在普通 user 支内恰好一次（闸门之外）', () => {
    assert.equal(count(app, /<UserWallet \/>/g), 1)
    assert.equal(count(appFlat, /path: 'wallet', element: <UserWallet \/>/g), 1)
    assert.equal(count(userBlock, /path: 'wallet', element: <UserWallet \/>/g), 1)
    // 旧的 app/user 支内子路由（'orders' / 'payment-accounts' / 'wallet' 裸段）必须绝迹；
    // 唯一允许的裸段是上面的钱包。
    assert.equal(count(appFlat, /path: '(orders|payment-accounts)'/g), 0, '旧的管理面裸子路由必须移除')
    assert.equal(count(appFlat, /path: 'wallet'/g), 1, '钱包裸子路由只允许 user 支内这一处')
    assert.equal(count(appFlat, /element: <(Orders|PaymentAccounts|UserWallet) \/>/g), 3)
  })

  it('/user/redemptions 兼容路由恰好一处且只重定向（不渲染旧页）', () => {
    assert.equal(count(app, /path: 'redemptions'/g), 1)
    assert.equal(count(app, /<Navigate to="\/user\/wallet\?tab=redeem" replace \/>/g), 1)
    assert.ok(userBlock.includes(`path: 'redemptions', element: <Navigate to="/user/wallet?tab=redeem" replace />`))
    // 旧页面既不再被 import，也不该再存在（兼容路由不得渲染旧兑换页）。
    assert.equal(count(app, /UserRedemptions/g), 0)
    assert.equal(existsSync(new URL('pages/user/redemptions.tsx', srcRoot)), false, '旧兑换页必须删除')
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

// —— 钱包页的源码形状 tripwire（spec「门禁」第 2 条）——
// 本仓库没有 DOM 测试环境，所以「支付关闭不请求 methods / 不渲染充值字段 / 不渲染订单
// 列表」只能靠源码形状断言守：渠道查询的 enabled、充值块的 payment 守卫、订单渲染的位置、
// 兑换两侧不被守卫包住。模式矩阵本身由 wallet-mode.test.ts 守。
const walletSrc = read('pages/user/wallet.tsx')
const walletLines = walletSrc.split('\n')
const countStr = (src: string, needle: string) => src.split(needle).length - 1
// 三处守卫块：充值面板 / 订单记录 / 权威金额确认层。用不同的守卫字面量区分，避免误取。
const topupGuard = blockOf(walletSrc, '{payment && canTopup && (')
const ordersGuard = blockOf(walletSrc, '{payment && canViewOrders && (')
const confirmGuard = blockOf(walletSrc, '{payment && created && (')

describe('钱包页：支付关闭不留下任何充值痕迹（源码形状）', () => {
  it('渠道查询只有一处，且 query 里带 enabled: payment', () => {
    assert.equal(countStr(walletSrc, 'listPaymentMethods'), 1, 'listPaymentMethods 只允许调用一次')
    const chanLines = walletLines.filter(l => l.includes('listPaymentMethods'))
    assert.equal(chanLines.length, 1)
    assert.ok(chanLines[0].includes('enabled: payment'), '渠道查询必须由支付开关使能，关闭时不发请求')
  })

  it('订单查询同样由支付开关 + 可见性使能（关闭/零渠道都不请求）', () => {
    assert.ok(walletSrc.includes('enabled: payment && canViewOrders'))
    // ?order= 轮询的上下文也要求支付开启：支付关时 orderForced 恒 false，locate/focus 都不发。
    assert.ok(walletSrc.includes("const orderForced = payment && focusOrderNo !== ''"))
    assert.ok(walletSrc.includes('enabled: orderForced'))
  })

  it('充值区块整体由 payment 守卫包住（含渠道/金额/汇率/下单按钮）', () => {
    for (const marker of [
      'user.wallet.topupTitle',
      'user.wallet.channelLabel',
      'user.wallet.amountPlaceholder',
      'user.wallet.estimatePayLabel',
      'user.wallet.payNow',
      'user.wallet.amountInvalid',
    ]) {
      assert.ok(topupGuard.includes(marker), `充值守卫内缺少 ${marker}`)
      assert.equal(countStr(walletSrc, marker), countStr(topupGuard, marker), `${marker} 不得在守卫外出现`)
    }
    // 充值 Tab 本身也只在可用集合里有 topup 时才出现（零渠道没有死 Tab）。
    assert.equal(countStr(walletSrc, '{canTopup && <TabsTrigger value="topup">'), 1)
  })

  it('订单列表只在订单守卫内渲染（空态/骨架/错误/分页总数）', () => {
    // 只取渲染处的形态标记：死胡同守卫 effect 与轮询刷新按钮读的是同一批字段，
    // 但它们不是"渲染订单列表"，不该被这条断言误伤。
    for (const marker of [
      'ordersQ.isError ? (',
      'ordersQ.isLoading ? (',
      'ordersQ.error as Error',
      'ordersQ.data?.total',
      'user.wallet.emptyOrders',
      'user.wallet.table.paidAt',
      'user.wallet.table.actions',
      'user.wallet.filterStatus',
    ]) {
      assert.ok(ordersGuard.includes(marker), `订单守卫内缺少 ${marker}`)
      assert.equal(countStr(walletSrc, marker), countStr(ordersGuard, marker), `${marker} 不得在守卫外渲染`)
    }
    assert.equal(countStr(walletSrc, '{canViewOrders && <TabsTrigger value="orders">'), 1)
  })

  it('权威金额确认层同样在 payment 守卫内（支付被关掉不留下旧充值内容）', () => {
    assert.ok(confirmGuard.includes('user.wallet.confirmPayAmount'))
    assert.ok(confirmGuard.includes('created.pay_amount'))
    assert.equal(countStr(walletSrc, 'user.wallet.confirmPayAmount'), countStr(confirmGuard, 'user.wallet.confirmPayAmount'))
  })

  it('运行时：切回标签页时定向重取开关（只失效 FEATURES_QUERY_KEY，不碰整前缀）', () => {
    assert.ok(walletSrc.includes('FEATURES_QUERY_KEY'), '重取必须用与 useFeatures 同源的精确键')
    assert.ok(walletSrc.includes("addEventListener('focus'"))
    assert.ok(walletSrc.includes("addEventListener('visibilitychange'"))
    assert.doesNotMatch(walletSrc, /invalidateQueries\(\{\s*queryKey:\s*\[\s*'user'\s*\]/, '不得整前缀失效开关缓存')
  })
})

describe('钱包页：兑换可用性与支付开关无关（源码形状）', () => {
  it('兑换 API 调用各一处，且不在任何 payment 守卫内', () => {
    assert.equal(countStr(walletSrc, 'listUserRedemptions'), 1)
    assert.equal(countStr(walletSrc, 'userApi.redeem('), 1)
    for (const [name, block] of [['充值', topupGuard], ['订单', ordersGuard], ['确认层', confirmGuard]] as const) {
      assert.equal(countStr(block, 'listUserRedemptions'), 0, `${name}守卫内不得有兑换记录请求`)
      assert.equal(countStr(block, 'userApi.redeem('), 0, `${name}守卫内不得有兑换 mutation`)
    }
  })

  it('兑换表单与兑换记录面板都不在 payment 守卫内', () => {
    const topupEnd = walletSrc.indexOf(topupGuard) + topupGuard.length
    const ordersEnd = walletSrc.indexOf(ordersGuard) + ordersGuard.length
    // 同级而非嵌套：兑换面板必须落在充值守卫块之后，兑换记录面板必须落在订单守卫块之后。
    // （若有人把兑换塞进守卫里，indexOf 会小于守卫块末尾。）
    const redeemAt = walletSrc.indexOf('<TabsContent value="redeem">')
    assert.notEqual(redeemAt, -1, '找不到兑换面板')
    assert.ok(redeemAt > topupEnd, '兑换面板不能被充值守卫包住')
    const redemptionRecordsAt = walletSrc.indexOf('<TabsContent value="redemptions">')
    assert.notEqual(redemptionRecordsAt, -1, '找不到兑换记录面板')
    assert.ok(redemptionRecordsAt > ordersEnd, '兑换记录面板不能被订单守卫包住')
    for (const marker of ['user.wallet.codeLabel', 'user.wallet.successTitle', 'user.wallet.emptyRedemptions']) {
      assert.equal(countStr(topupGuard, marker), 0, `${marker} 不该在充值守卫内`)
      assert.equal(countStr(ordersGuard, marker), 0, `${marker} 不该在订单守卫内`)
    }
    assert.equal(countStr(walletSrc, 'user.wallet.emptyRedemptionsDesc'), 1)
  })
})
