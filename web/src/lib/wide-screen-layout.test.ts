// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 大屏布局（spec 2026-11-18）：源码形状 tripwire。
//
// 本轮的语义全部落在 class 字符串上——没有 jsdom/RTL，没有能接住「内容列多宽」
// 的渲染测试，几何只能靠 Playwright 协议验（spec §9.2）。这里钉住的是**不许回退的
// 形状**：壳层不得再出现统一上限、容器查询阈值只允许一个魔数、稀疏页必须有页面级
// 上限、key-value 行不得退回 `justify-between`。任何一条被「顺手简化」都会红。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, relative } from 'node:path'

const read = (rel: string) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const shell = read('../components/app-shell.tsx')
const settings = read('../pages/settings.tsx')
const profile = read('../pages/user/profile.tsx')
const overview = read('../pages/user/overview.tsx')
const models = read('../pages/user/models.tsx')
const wallet = read('../pages/user/wallet.tsx')
const balanceCard = read('../components/balance-warning-card.tsx')

const SRC_ROOT = fileURLToPath(new URL('..', import.meta.url))

/** 递归收集 `src` 下的 .ts/.tsx（排除测试与本文件自身）。 */
function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, name.name)
    if (name.isDirectory()) walk(full, out)
    else if (/\.tsx?$/.test(name.name) && !/\.test\.tsx?$/.test(name.name)) out.push(full)
  }
  return out
}

describe('壳层：内容列没有统一上限，且与 header 同缩进', () => {
  it('不含 max-w-[1440px]（回归 = 所有页面重新留 428px 空白）', () => {
    assert.equal(shell.includes('max-w-[1440px]'), false)
  })

  it('内容列是裸 flex 列（mx-auto 一并去掉，避免被读成「还有约束」）', () => {
    assert.match(shell, /<div className="flex w-full flex-col gap-4 md:gap-6">/)
  })

  it('内容区留 @container/main（所有 @[…]/main: 变体的查询容器）+ 与 header 对齐的 px-3/lg:px-5', () => {
    assert.ok(shell.includes('@container/main'))
    assert.ok(shell.includes('px-3 pb-6 pt-7 lg:px-5 lg:pb-8 lg:pt-9'))
  })
})

describe('容器查询阈值：全站只允许 @[2000px]/main 一个魔数', () => {
  it('src 下所有 @[Npx]/main: 的 N 恒为 2000', () => {
    const seen = new Set<string>()
    for (const file of walk(SRC_ROOT)) {
      const text = readFileSync(file, 'utf8')
      for (const m of text.matchAll(/@\[(\d+)px\]\/main:/g)) seen.add(m[1])
    }
    assert.deepEqual([...seen], ['2000'], `出现了别的阈值：${[...seen].join(', ')}`)
  })

  it('第一轮方案的 @[1600px] 已绝迹（否则设置页单行分组会重新变成「半卡空白」）', () => {
    const hits: string[] = []
    for (const file of walk(SRC_ROOT)) {
      if (readFileSync(file, 'utf8').includes('1600px')) hits.push(relative(SRC_ROOT, file))
    }
    assert.deepEqual(hits, [])
  })
})

describe('稀疏页：页面级上限只在超宽屏生效', () => {
  it('设置页 / 个人中心 6xl，用户总览 5xl，钱包 6xl', () => {
    assert.ok(settings.includes('mx-auto w-full space-y-6 @container/page @[2000px]/main:max-w-6xl'))
    assert.ok(profile.includes('mx-auto w-full space-y-6 @[2000px]/main:max-w-6xl'))
    assert.ok(overview.includes('mx-auto w-full space-y-6 @[2000px]/main:max-w-5xl'))
    assert.ok(wallet.includes('mx-auto w-full space-y-6 @[2000px]/main:max-w-6xl'))
  })

  it('1920 及以下不得收窄：上限必须挂 @[2000px]/main 前缀，不能裸 max-w', () => {
    // 注释里会提到类名（本文档也在解释这件事），先剥注释再扫，避免自伤。
    const stripComments = (s: string) => s.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/[^\n]*/g, '')
    for (const [name, text] of [['settings', settings], ['profile', profile], ['overview', overview], ['wallet', wallet]] as const) {
      const code = stripComments(text)
      for (const m of code.matchAll(/(^|["\s])(max-w-6xl|max-w-5xl)/g)) {
        const before = code.slice(Math.max(0, (m.index ?? 0) - 24), (m.index ?? 0) + m[1].length)
        assert.ok(before.includes('@[2000px]/main:'), `${name} 出现了裸的上限：…${before}`)
      }
    }
  })
})

describe('key-value 行：不再用 justify-between 把两端拉开', () => {
  it('SettingRow 改成 [1fr_24rem] 网格，文本块 72ch，邮件页分栏按页面宽度', () => {
    assert.ok(settings.includes('lg:grid-cols-[minmax(0,1fr)_24rem]'))
    assert.ok(settings.includes('max-w-[72ch]'))
    assert.ok(settings.includes('@[1400px]/page:grid-cols-2'))
    assert.equal(settings.includes('divide-y'), false, '行分隔线走逐项 border-b，不能用 divide-y')
  })

  it('InfoRow 是 [minmax(7rem,auto)_1fr] 网格（值左对齐，间距恒 ≤16px）', () => {
    assert.ok(profile.includes('grid-cols-[minmax(7rem,auto)_minmax(0,1fr)]'))
    assert.equal(profile.includes('flex items-center justify-between gap-3'), false)
  })

  it('个人中心 2×2：余额预警与修改密码并排', () => {
    assert.match(profile, /grid grid-cols-1 items-start gap-5 xl:grid-cols-2/)
  })
})

describe('表单控件封顶：卡片保持等宽，控件的宽度上限下移', () => {
  it('余额预警卡不再有卡片级 max-w-xl（否则同排两卡 798/576 参差）', () => {
    assert.equal(balanceCard.includes('max-w-xl'), false)
    assert.ok(balanceCard.includes('flex min-w-0 max-w-sm items-center gap-2'))
  })

  it('修改密码的输入区有 max-w-xl（1920 下不把密码框拉到 765px）', () => {
    assert.ok(profile.includes('max-w-xl space-y-3'))
  })

  it('模型页保留第 4 轨（重复集合，条目数可增长）', () => {
    assert.ok(models.includes('@[2000px]/main:grid-cols-4'))
  })
})
