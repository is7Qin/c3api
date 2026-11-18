// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// RetryCard 渲染层守卫：`null`（服务端「分母为 0」）必须渲染 `—`，确定的 0 必须
// 渲染 `0.0%`。lib/ops-stats 的单测只能证明 formatter 本体；这里断言**页面实际
// 使用的组件**，抓的是「页面把 null 折叠成 0」「卡片不再走 DASH」这类回归。
//
// 为什么是 .ts 而不是 .tsx：web 的测试由 `node --test` 直接执行（无 JSX loader），
// Node 的类型剥离不处理 JSX；因此用 createElement 渲染，TSX 交由 Vite 的
// ssrLoadModule 现场转译（vite 已是运行时依赖，零新增依赖）。
import assert from 'node:assert/strict'
import path from 'node:path'
import { after, before, test } from 'node:test'
import { createElement } from 'react'
import type { ComponentType } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import react from '@vitejs/plugin-react'
import { createServer } from 'vite'
import type { ViteDevServer } from 'vite'

// i18n 在模块求值时读 localStorage（真实浏览器语义）。测试进程里给一个最小替身，
// 让组件拿到真实的 `t()`（缺了它 react-i18next 只输出 key，且会打警告）。
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
let RetryCard: ComponentType<{ q: unknown }>

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
  // 初始化全局 i18n（页面入口在 main.tsx 里做同样的事），再取真实卡片组件。
  const i18n = await server.ssrLoadModule('/src/lib/i18n.ts')
  await i18n.default.init
  const mod = await server.ssrLoadModule('/src/components/retry-card.tsx')
  RetryCard = mod.RetryCard
})

after(async () => {
  await server.close()
})

type RetryResponse = {
  routes: number
  queried_at: string
  request_count: number
  attempt_total: number
  retry_rate: number | null
  avg_attempts: number | null
  exhausted_rate: number | null
  retry_success_rate: number | null
  by_reason: { outcome: string; chain_count: number }[]
}

function retryData(overrides: Partial<RetryResponse> = {}): RetryResponse {
  return {
    routes: 1,
    queried_at: '2026-11-18T00:00:00Z',
    request_count: 3,
    attempt_total: 4,
    retry_rate: null,
    avg_attempts: 2.5,
    exhausted_rate: 0,
    retry_success_rate: null,
    by_reason: [],
    ...overrides,
  }
}

function render(data: RetryResponse): string {
  const q = { data, isError: false, isLoading: false, isRefetchError: false }
  return renderToStaticMarkup(createElement(RetryCard, { q }))
}

// countText 数单元格**文本**（`>值<`）；title 属性里也有同一个值，不能算进去。
function countText(html: string, value: string): number {
  return html.split(`>${value}<`).length - 1
}

test('null 比值渲染 —（不是 0.0%）：retry_rate 与 retry_success_rate', () => {
  const html = render(retryData({ retry_rate: null, retry_success_rate: null, exhausted_rate: 0 }))
  assert.equal(countText(html, '—'), 2, '两个 null 比值各一格 —')
  assert.equal(countText(html, '0.0%'), 1, '只有 exhausted_rate 的确定 0 走 0.0%')
  assert.ok(html.includes('>2.5<'), 'avg_attempts = 2.5 原样渲染（不四舍五入成 2 或 3）')
})

test('确定的 0 渲染 0.0%（不是 —）：retry_rate 与 retry_success_rate', () => {
  const html = render(retryData({ retry_rate: 0, retry_success_rate: 0, exhausted_rate: 0 }))
  assert.equal(countText(html, '—'), 0, '没有分母为 0 的比值 ⇒ 一格 — 都不该出现')
  assert.equal(countText(html, '0.0%'), 3, '三个确定 0 全部 0.0%')
  assert.ok(html.includes('>2.5<'))
})

test('0 与 null 并存：同一次渲染里两者不可互换', () => {
  const html = render(retryData({ retry_rate: 0, retry_success_rate: null, exhausted_rate: 0 }))
  assert.equal(countText(html, '0.0%'), 2, 'retry_rate / exhausted_rate = 0.0%')
  assert.equal(countText(html, '—'), 1, 'retry_success_rate 的分母为 0 ⇒ —')
})

