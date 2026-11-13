// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 供应商功能开关的前端判定与接线。
//   - 纯函数（supplier-visibility.ts）：开/关两态精确断言（数组逐项、开启态引用不变）。
//   - 接线 tripwire：本项目没有 jsdom / 渲染测试，「开关有没有接到每一处、路由闸门是否
//     在角色门控之外」只能读源码文本断言（与 proxy-page.test.ts 同款）。它们不是行为测试，
//     只守最容易静默回归的形状：闸门分支顺序、路由嵌套、各页面的判定调用、开关只读一次。
// 运行：`node --test`。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import {
  ADMIN_SUPPLIER_PATH,
  adminNavFor,
  assignableRoles,
  filterableRoles,
  isOpsStatVisible,
  isSupplierPath,
  showSupplierNav,
} from './supplier-visibility.ts'

describe('isSupplierPath', () => {
  it('管理面结算工作台与供应商控制台整棵子树为真', () => {
    assert.equal(isSupplierPath('/app/supplier'), true)
    assert.equal(isSupplierPath('/user/supplier'), true)
    assert.equal(isSupplierPath('/user/supplier/accounts'), true)
    assert.equal(isSupplierPath('/user/supplier/proxies'), true)
  })

  it('前缀相同但不是同一段的路径、以及两台根路径为假', () => {
    assert.equal(isSupplierPath('/user/supplierx'), false)
    assert.equal(isSupplierPath('/app/supplier-x'), false)
    assert.equal(isSupplierPath('/user'), false)
    assert.equal(isSupplierPath('/app'), false)
  })
})

describe('adminNavFor', () => {
  const nav = [
    { to: '/app/dashboard', key: 'a' },
    { to: '/app/pricing', key: 'b' },
    { to: ADMIN_SUPPLIER_PATH, key: 'c' },
    { to: '/app/settings', key: 'd' },
  ] as const

  it('开启 ⇒ 原样返回同一引用', () => {
    assert.equal(adminNavFor(nav, true), nav)
  })

  it('关闭 ⇒ 只去掉结算工作台，其余保序，且不改入参', () => {
    assert.deepEqual(adminNavFor(nav, false), [
      { to: '/app/dashboard', key: 'a' },
      { to: '/app/pricing', key: 'b' },
      { to: '/app/settings', key: 'd' },
    ])
    assert.equal(nav.length, 4)
  })

  it('管理面结算工作台路径与路由字面量一致', () => {
    assert.equal(ADMIN_SUPPLIER_PATH, '/app/supplier')
  })
})

describe('showSupplierNav（角色 × 开关满格）', () => {
  it('开启：platform_admin / supplier 可见，user / 未知角色不可见', () => {
    assert.equal(showSupplierNav('platform_admin', true), true)
    assert.equal(showSupplierNav('supplier', true), true)
    assert.equal(showSupplierNav('user', true), false)
    assert.equal(showSupplierNav(undefined, true), false)
  })

  it('关闭：任何角色都不可见', () => {
    assert.equal(showSupplierNav('platform_admin', false), false)
    assert.equal(showSupplierNav('supplier', false), false)
  })
})

describe('filterableRoles / assignableRoles', () => {
  it('筛选项：开启三项（原顺序），关闭去掉 supplier', () => {
    assert.deepEqual(filterableRoles(true), ['platform_admin', 'user', 'supplier'])
    assert.deepEqual(filterableRoles(false), ['platform_admin', 'user'])
  })

  it('表单项：开启三项', () => {
    assert.deepEqual(assignableRoles(true), ['platform_admin', 'user', 'supplier'])
  })

  it('表单项：关闭但正在编辑的用户就是 supplier ⇒ 保留 supplier（下拉能显示当前值）', () => {
    assert.deepEqual(assignableRoles(false, 'supplier'), ['platform_admin', 'user', 'supplier'])
  })

  it('表单项：关闭且当前角色不是 supplier（或新建）⇒ 不提供 supplier', () => {
    assert.deepEqual(assignableRoles(false, 'user'), ['platform_admin', 'user'])
    assert.deepEqual(assignableRoles(false), ['platform_admin', 'user'])
  })
})

describe('isOpsStatVisible', () => {
  it('capacity_blocked 只在开启时展示，其余统计项不受影响', () => {
    assert.equal(isOpsStatVisible('capacity_blocked', false), false)
    assert.equal(isOpsStatVisible('capacity_blocked', true), true)
    assert.equal(isOpsStatVisible('disk_free_bytes', false), true)
  })
})

// —— 接线 tripwire（源码形状）——

const srcRoot = new URL('../', import.meta.url)
const read = (rel: string) => readFileSync(new URL(rel, srcRoot), 'utf8')
const count = (src: string, re: RegExp) => (src.match(re) ?? []).length
const shell = read('components/app-shell.tsx')
const app = read('App.tsx')
// 路由结构断言只看嵌套与顺序，不看排版：空白归一后匹配，单行与多行写法等价。
const appFlat = app.replace(/\s+/g, ' ')
const accounts = read('pages/accounts.tsx')
const users = read('pages/users.tsx')
const proxyPage = read('components/proxy-pool-page.tsx')
const ops = read('pages/ops.tsx')
const featuresHook = read('lib/use-features.ts')

describe('接线：AppShell（菜单与面包屑）', () => {
  it('顶层取开关；管理组经 adminNavFor、供应商组经 showSupplierNav', () => {
    assert.ok(shell.includes('useFeatures('))
    assert.ok(shell.includes('adminNavFor(adminNav, supplier)'))
    assert.ok(shell.includes('showSupplierNav(me?.Role, supplier)'))
  })

  it('面包屑传入开关，且在任何路径分支之前先挡供应商路径', () => {
    assert.match(shell, /breadcrumbFor\(location\.pathname, supplier\)/)
    const at = shell.indexOf('function breadcrumbFor(')
    assert.notEqual(at, -1, '找不到 breadcrumbFor')
    const body = shell.slice(at, shell.indexOf('\n}\n', at))
    const guard = 'if (!supplier && isSupplierPath(pathname)) return null'
    const firstBranch = body.indexOf('if (pathname.startsWith')
    assert.ok(body.includes(guard), 'breadcrumbFor 缺少供应商路径守卫')
    assert.notEqual(firstBranch, -1, '找不到 breadcrumbFor 的路径分支')
    assert.ok(body.indexOf(guard) < firstBranch, '守卫必须先于 /app、/user 分支')
  })
})

describe('接线：路由闸门（App.tsx）', () => {
  it('两条供应商路由的 element 都是闸门，各组件只挂一次', () => {
    assert.equal(count(appFlat, /element: <SupplierFeatureGate \/>/g), 2)
    assert.equal(count(appFlat, /path: 'app\/supplier'/g), 1)
    assert.equal(count(appFlat, /element: <SupplierAdmin \/>/g), 1)
    assert.equal(count(appFlat, /element: <RequireAdmin \/>/g), 2, 'app 支 + app/supplier 支')
    assert.equal(count(appFlat, /element: <RequireSupplier \/>/g), 1)
  })

  it('/app/supplier 是独立兄弟支：闸门在外，RequireAdmin 在支内、位于 SupplierAdmin 之前', () => {
    assert.match(appFlat, /path: 'app\/supplier', element: <SupplierFeatureGate \/>/)
    const anchor = "path: 'app/supplier'"
    const s = appFlat.indexOf(anchor)
    const e = appFlat.indexOf('element: <SupplierAdmin />')
    assert.ok(e > s, 'SupplierAdmin 必须在 app/supplier 支之后出现')
    const branch = appFlat.slice(s + anchor.length, e)
    assert.doesNotMatch(branch, /path: '/, 'SupplierAdmin 只能位于 app/supplier 支内')
    assert.ok(branch.includes('element: <RequireAdmin />'), '角色门控必须在该支内、闸门之内')
  })

  it('/user/supplier：闸门在外，RequireSupplier 在闸门与 SupplierScope 之间', () => {
    const anchor = "path: 'supplier', element: <SupplierFeatureGate />"
    assert.match(appFlat, /path: 'supplier', element: <SupplierFeatureGate \/>/)
    const u = appFlat.indexOf(anchor)
    const v = appFlat.indexOf('element: <SupplierScope />')
    assert.ok(v > u, 'SupplierScope 必须在闸门之内')
    assert.ok(appFlat.slice(u, v).includes('element: <RequireSupplier />'))
  })

  it('闸门分支顺序：未登录 → 加载中 → 关闭 404 → 放行', () => {
    const i = app.indexOf('function SupplierFeatureGate')
    assert.notEqual(i, -1, '找不到 SupplierFeatureGate')
    const j = app.indexOf('\nfunction ', i + 1)
    const body = app.slice(i, j === -1 ? undefined : j)
    // 钩子必须无条件先调（rules of hooks），解构出的 resolved 不是分支：分支段从
    // useFeatures() 之后算起。
    const hook = body.indexOf('useFeatures()')
    assert.notEqual(hook, -1, '闸门必须经 useFeatures() 取开关')
    const branches = body.slice(hook)
    const tokens = ['getToken', 'resolved', '<NotFound />', '<Outlet />']
    const pos = tokens.map((tk) => branches.indexOf(tk))
    pos.forEach((p, k) => assert.notEqual(p, -1, `闸门缺少 ${tokens[k]}`))
    for (let k = 1; k < pos.length; k++) {
      assert.ok(pos[k] > pos[k - 1], `${tokens[k - 1]} 必须先于 ${tokens[k]}`)
    }
  })
})

describe('接线：页面内容面', () => {
  it('账号页：归属字段受开关约束，草稿初值为空对象，提示文案按开关分流', () => {
    assert.ok(accounts.includes('!isSupplierScope && supplier'))
    assert.ok(accounts.includes('useState<AccountDraft>({})'))
    assert.match(accounts, /t\(!isSupplierScope && !supplier \? 'accounts\.ungroupedNoticePlatform' : 'accounts\.ungroupedNotice'\)/)
  })

  it('用户页：筛选与表单角色都来自判定函数，不再有本地角色常量', () => {
    assert.ok(users.includes('...filterableRoles(supplier)'))
    assert.ok(users.includes('assignableRoles(supplier, editing?.Role)'))
    assert.ok(!users.includes('const ROLES'))
  })

  it('代理池：归属列表头与单元格都受开关约束，关闭时副标题换平台文案', () => {
    assert.ok(proxyPage.includes('!isSupplier && supplier'))
    assert.equal(count(proxyPage, /\{showOwner &&/g), 2, '表头与单元格必须同一条件，否则列错位')
    assert.ok(proxyPage.includes('proxies.subtitleAdminPlatform'))
  })

  it('运维页：统计项经 isOpsStatVisible 过滤，空态看过滤后的同一份', () => {
    assert.ok(ops.includes('isOpsStatVisible(k, supplier)'))
    assert.ok(ops.includes('entries.length === 0'))
    assert.ok(!ops.includes('Object.keys(w.stats ?? {}).length === 0'), '空态不得再看未过滤的 stats')
  })
})

describe('接线：开关只读一次', () => {
  // 扫描面：src 下全部 .ts/.tsx（测试文件与生成的 schema.d.ts 除外）。
  const files = (readdirSync(srcRoot, { recursive: true }) as string[])
    .map((p) => p.replaceAll('\\', '/'))
    .filter((p) => /\.tsx?$/.test(p) && !p.endsWith('.test.ts') && !p.endsWith('schema.d.ts'))

  it('hook 固定同一 query key、页面生命周期内不过期也不回收、未登录不请求', () => {
    assert.ok(featuresHook.includes('staleTime: Infinity'))
    assert.ok(featuresHook.includes('gcTime: Infinity'))
    assert.ok(featuresHook.includes("['user', 'features']"))
    assert.ok(featuresHook.includes('enabled: Boolean(userAuth.getToken())'))
  })

  it('扫描面非空且覆盖页面与路由文件（glob 坏掉时不许静默全绿）', () => {
    for (const must of ['App.tsx', 'components/app-shell.tsx', 'pages/accounts.tsx', 'lib/use-features.ts']) {
      assert.ok(files.includes(must), `扫描面缺少 ${must}`)
    }
  })

  it('没有任何 [\'user\'] 整前缀失效（会命中开关的缓存键）', () => {
    for (const f of files) {
      assert.doesNotMatch(read(f), /invalidateQueries\(\{\s*queryKey:\s*\[\s*'user'\s*\]\s*[,}]/, f)
    }
  })

  it('开关端点只经 useFeatures 读取', () => {
    const readers = files.filter((f) => read(f).includes('userApi.features('))
    assert.deepEqual(readers, ['lib/use-features.ts'])
  })
})
