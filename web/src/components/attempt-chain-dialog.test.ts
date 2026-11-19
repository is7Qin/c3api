// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 请求链对话框 + request_id 单元格的渲染层守卫（spec 2026-11-18 §6 S3 的前端半边）。
//
// 断言的是**页面实际使用的组件**，抓的三类回归：
//   - `ordinal=null` 必须渲染 `—`（不得折成 0 / 空字符串）+ 「无序号」徽标；
//   - `truncated` / 空态 / 采样提示必须真的出现在 DOM 里（不是只存在类型里）；
//   - request_id 单元格必须截断到 ~12 字符且 `title` 带全文（点它是唯一入口）。
//
// 为什么是 .ts + createElement：web 的测试由 `node --test` 直接执行（无 JSX loader），
// TSX 交由 Vite 的 ssrLoadModule 现场转译（照 retry-card.test.ts 的做法）。
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { after, before, test } from 'node:test'
import { createElement } from 'react'
import type { ComponentType } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import react from '@vitejs/plugin-react'
import { createServer } from 'vite'
import type { ViteDevServer } from 'vite'

// i18n 在模块求值时读 localStorage（真实浏览器语义）；测试进程里给一个最小替身。
// 文案断言一律走 `i18n.t(key)`（语言无关）——不硬编码中/英文本，换 locale 不会假红。
const store = new Map<string, string>()
globalThis.localStorage = {
  getItem: (k: string) => store.get(k) ?? null,
  setItem: (k: string, v: string) => { store.set(k, String(v)) },
  removeItem: (k: string) => { store.delete(k) },
  clear: () => { store.clear() },
  key: (i: number) => [...store.keys()][i] ?? null,
  get length() { return store.size },
} as Storage

const webRoot = path.resolve(import.meta.dirname, '../..')

let server: ViteDevServer
let AttemptChainBody: ComponentType<{ q: unknown }>
let attemptChainTarget: (requestID: string, createdAt: string) => { requestID: string; createdAt: string; from: string; to: string }
let RequestIdCell: ComponentType<{ id?: string; onOpen?: (id: string) => void }>
let REQUEST_ID_SHOWN_CHARS: number
let t: (key: string) => string = () => ''

before(async () => {
  server = await createServer({
    root: webRoot,
    configFile: false,
    logLevel: 'error',
    plugins: [react()],
    resolve: { alias: { '@': path.resolve(webRoot, 'src') } },
    server: { middlewareMode: true },
    appType: 'custom',
  })
  const i18n = await server.ssrLoadModule('/src/lib/i18n.ts')
  await i18n.default.init
  t = (key: string) => i18n.default.t(key as never)
  const dialog = await server.ssrLoadModule('/src/components/attempt-chain-dialog.tsx')
  AttemptChainBody = dialog.AttemptChainBody
  attemptChainTarget = dialog.attemptChainTarget
  const display = await server.ssrLoadModule('/src/components/log-display.tsx')
  RequestIdCell = display.RequestIdCell
  REQUEST_ID_SHOWN_CHARS = display.REQUEST_ID_SHOWN_CHARS
})

after(async () => {
  await server.close()
})

type Hop = {
  source: 'attempt' | 'err' | 'usage'
  ordinal: number | null
  ordinal_unknown: boolean
  account_id: number
  lane: string
  format: string
  status_code: number
  outcome: string
  error_type: string
  commit_state: string | null
  error_message: string | null
  latency_ms: number
  created_at: string
}

type Chain = {
  request_id: string
  from: string
  to: string
  hops: Hop[]
  truncated: boolean
  sampling?: { dropped_total: number }
}

function hop(overrides: Partial<Hop> = {}): Hop {
  return {
    source: 'attempt',
    ordinal: 1,
    ordinal_unknown: false,
    account_id: 11,
    lane: 'primary',
    format: 'openai-chat',
    status_code: 500,
    outcome: '5xx',
    error_type: '5xx',
    commit_state: 'upstream_responded',
    error_message: 'boom',
    latency_ms: 40,
    created_at: '2026-11-18T10:00:00Z',
    ...overrides,
  }
}

function chainData(overrides: Partial<Chain> = {}): Chain {
  return {
    request_id: 'req-1',
    from: '2026-11-18T09:00:00Z',
    to: '2026-11-18T11:00:00Z',
    hops: [hop()],
    truncated: false,
    sampling: { dropped_total: 0 },
    ...overrides,
  }
}

function render(data: Chain | undefined): string {
  const q = { data, isError: false, isLoading: data === undefined, isRefetchError: false }
  return renderToStaticMarkup(createElement(AttemptChainBody, { q }))
}

// countText 数单元格**文本**（`>值<`）；title 属性里的同值不算。
function countText(html: string, value: string): number {
  return html.split(`>${value}<`).length - 1
}

test('ordinal=null 渲染 — 且带「无序号」徽标（不折成 0）', () => {
  const html = render(chainData({
    hops: [
      hop({ source: 'err', ordinal: null, ordinal_unknown: true, lane: '', outcome: '', status_code: 401 }),
      hop({ ordinal: 1 }),
    ],
  }))
  // 两跳里只有一小格 —（另一跳的 ordinal 是 1）。账号/耗时等其它 0 值走它们自己的 —。
  assert.equal(countText(html, '—'), 1, 'ordinal=null 必须渲染 —（且只有那一格是纯 —）')
  assert.ok(html.includes(t('logs.chain.ordinalUnknown')), 'ordinal_unknown 必须显示徽标文案')
  assert.ok(html.includes('>1<'), '有 ordinal 的那一跳原样显示 1')
})

test('truncated=true 渲染截断提示（不静默）', () => {
  const html = render(chainData({ truncated: true }))
  assert.ok(html.includes(t('logs.chain.truncated')), 'must show the truncation notice')
})

test('truncated=false 不渲染截断提示', () => {
  const html = render(chainData({ truncated: false }))
  assert.ok(!html.includes(t('logs.chain.truncated')))
})

test('空链渲染空态（不是空白）', () => {
  const html = render(chainData({ hops: [] }))
  assert.ok(html.includes(t('logs.chain.emptyTitle')), 'empty hops ⇒ empty state title')
  assert.ok(html.includes(t('logs.chain.emptyDesc')))
})

test('采样提示恒在；dropped_total>0 时附丢弃数', () => {
  const clean = render(chainData({ sampling: { dropped_total: 0 } }))
  assert.ok(clean.includes(t('logs.chain.samplingHint')), 'sampling honesty hint is always shown')
  const dropped = render(chainData({ sampling: { dropped_total: 7 } }))
  assert.ok(dropped.includes('7'), 'dropped_total>0 must surface the number')
})

test('三源标签按 source 区分（attempt/err/usage 各自文案）', () => {
  const html = render(chainData({
    hops: [
      hop({ source: 'attempt', ordinal: 1 }),
      hop({ source: 'err', ordinal: null, ordinal_unknown: true }),
      hop({ source: 'usage', ordinal: null }),
    ],
  }))
  for (const key of ['logs.chain.sourceAttempt', 'logs.chain.sourceErr', 'logs.chain.sourceUsage']) {
    assert.ok(html.includes(t(key)), `missing source label ${key}`)
  }
})

test('request_id 单元格截断到约 12 字符 + title 全文', () => {
  const id = 'abcdefghijklmnopqrstuvwxyz'
  const html = renderToStaticMarkup(createElement(RequestIdCell, { id, onOpen: () => {} }))
  assert.ok(html.includes(`${id.slice(0, REQUEST_ID_SHOWN_CHARS)}…`), 'short form is the truncated prefix + ellipsis')
  assert.ok(html.includes(`title="${id}"`), 'title carries the full request id')
  assert.ok(!html.includes(`>${id}<`), 'the full id must not be the visible text')
})

// 截断长度必须**精确**是 12（不是「约 12」）：只断言「含省略号」时把
// REQUEST_ID_SHOWN_CHARS 从 12 改成 20 仍然绿（20 字符的 id 正好不被截断，也就
// 没有省略号……松断言同样放过）。这里用 20 字符 id 断言可见文本恰好是前 12 字符
// + 省略号，并直接钉住常量值。
test('request_id 截断长度精确为 12 字符', () => {
  const id = 'abcdefghijklmnopqrst' // 20 字符
  assert.ok(id.length > 12)
  const html = renderToStaticMarkup(createElement(RequestIdCell, { id, onOpen: () => {} }))
  assert.ok(
    html.includes(`>${id.slice(0, 12)}…</button>`),
    `可见文本必须恰好是前 12 字符 + 省略号（不是 12→20 那种松绿）：${html}`,
  )
  assert.equal(REQUEST_ID_SHOWN_CHARS, 12, '常量本身就是契约（日志页两个 Tab 共用）')
})

// 窗口必须是 created_at ∓ 24h（**毫秒级**相等，不是「差不多」）：±1h 会漏掉长流式
// + 多次换号的请求（attempt 行可能落在 created_at 之前数小时）。
test('attemptChainTarget 的窗口精确等于 created_at ∓ 24h', () => {
  const createdAt = '2026-11-18T10:00:30.123Z'
  const target = attemptChainTarget('req-1', createdAt)
  const at = new Date(createdAt).getTime()
  assert.equal(new Date(target.from).getTime(), at - 24 * 60 * 60 * 1000, 'from = created_at − 24h')
  assert.equal(new Date(target.to).getTime(), at + 24 * 60 * 60 * 1000, 'to = created_at + 24h')
  assert.equal(target.requestID, 'req-1')
  assert.equal(target.createdAt, createdAt)
})

test('attemptChainTarget 对非法日期退化为空窗口（交给服务端缺省）', () => {
  const target = attemptChainTarget('req-1', 'not-a-date')
  assert.equal(target.from, '')
  assert.equal(target.to, '')
})

// 唯一入口的**源码形状**守卫：两个 Tab（usage / errors）的 RequestIdCell 都必须
// 拿到 onOpen。momus 把 usage Tab 的 onOpen 去掉后 530 条测试 + tsc 全绿——渲染
// 测试抓不到「第三个调用点忘了接线」，只能钉源码形状。
test('两个 Tab 的 request_id 单元格都是可点的请求链入口', () => {
  const source = readFileSync(path.resolve(webRoot, 'src/pages/logs.tsx'), 'utf8')
  // 按开标签切段再取到 `/>`：JSX 属性里有 `=>`（箭头函数），正则 `[^>]*` 会在第一个
  // `>` 处断掉，故不能用单条正则匹配整个标签。
  const cells = source.split('<RequestIdCell').slice(1).map(part => part.slice(0, part.indexOf('/>')))
  assert.equal(cells.length, 2, 'usage 与 errors 两个 Tab 各一个入口单元格')
  for (const cell of cells) {
    assert.ok(cell.includes('onOpen='), `request_id 单元格缺 onOpen（入口失效）：${cell}`)
  }
  assert.ok(source.includes('attemptChainTarget(requestID'), '入口必须经 attemptChainTarget 建窗（±24h 单一来源）')
})
test('短 request_id 不截断（无省略号）', () => {
  const id = 'req-short'
  const html = renderToStaticMarkup(createElement(RequestIdCell, { id, onOpen: () => {} }))
  assert.ok(html.includes(`>${id}<`), 'short ids render verbatim')
  assert.ok(!html.includes('…'))
})
