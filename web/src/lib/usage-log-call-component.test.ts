// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 按次分量「可见性」的源码形状 tripwire（spec v3 §5.6）：本仓没有 jsdom / 渲染测试，
// 函数级单测看不见悬停窗的渲染结构，而这一行是否可见完全由组件里的外层门决定——
// 只能直接读源码断言形状。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (rel: string) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const PAGES = [
  { name: '管理面 logs.tsx', src: read('../pages/logs.tsx') },
  { name: '用户面 user/logs.tsx', src: read('../pages/user/logs.tsx') },
]

// 旧门：只认 token 四件套，按次行对 search / 0-token 图片行 100% 不可见。
const OLD_GATE = '{l.InputTokens || l.OutputTokens || l.CacheReadTokens || l.CacheCreationTokens ? ('
const NEW_GATE = /\{hasTokenUsage\(l\)\s*\|\|\s*\(\(l\.CallCount \?\? 0\) > 0 && l\.PricePerCallMillis != null\)\s*\?\s*\(/
// 行内容形状：数量不带单位，单位只随单价出现一次（`2 × $0.054/张`）。
const PER_CALL_LINE = /\{l\.CallCount\.toLocaleString\(\)\} × \{fmtPricePerUnit\(l\.PricePerCallMillis\)\}\/\{t\(/
// 去尾零必须顺序无关：先 /0+$/ 再 /\.$/（`/\.?0+$/` 作用在 '100' 上会削成 '1'）。
const TRIM_ZERO = /\.replace\(\/0\+\$\/, ''\)\.replace\(\/\\\.\$\/, ''\)/

describe('用量明细的按次分量（源码形状）', () => {
  it('★ 外层门必须认按次：旧 token-only 门已绝迹（新行可达性的唯一自动化防线）', () => {
    // 这是「按次行是否可达」的唯一自动化防线：本仓无 jsdom，没有渲染测试，
    // 函数级单测看不见外层门。openai-search 落日志时四个 token 字段恒 0
    // （forward_search.go 写 usageTuple{calls: 1}），旧门会把整个 ⓘ 与悬停窗挡掉，
    // 按次行与 logs.tokens.unitCall 键就成了死代码——这种回归只会在浏览器里静默消失，
    // 所以必须在源码里钉死「旧门不存在」。
    for (const { name, src } of PAGES) {
      assert.equal(src.includes(OLD_GATE), false, `${name}仍用 token-only 外层门，按次行不可达`)
    }
  })

  it('外层门是「token 任一非 0 或（按次计数 > 0 且有价快照）」', () => {
    for (const { name, src } of PAGES) {
      assert.match(src, NEW_GATE, `${name}缺新外层门 ${NEW_GATE}`)
      assert.ok(src.includes('(l.CallCount ?? 0) > 0'), `${name}外层门没把按次计数纳入`)
    }
  })

  it('★ token 全 0 时隐藏的五处内容都被 hasTokenUsage 守卫包住（拆任一处必红）', () => {
    for (const { name, src } of PAGES) {
      // 三个包裹点：cell 数字块、悬停窗 token 五行、总计行。
      const guards = src.match(/\{hasTokenUsage\(l\) &&/g) ?? []
      assert.equal(guards.length, 3, `${name}的 hasTokenUsage 包裹应为 3 处，实为 ${guards.length}`)
      // 逐个锚点回溯窗口内必须能看到守卫，防止「计数对但包错位置」。
      const anchors = [
        '<span className="space-y-0.5 text-xs text-right">',
        "{t('logs.tokens.input')}",
        "{t('logs.tokens.total')}",
      ]
      for (const a of anchors) {
        const i = src.indexOf(a)
        assert.notEqual(i, -1, `${name}找不到锚点 ${a}`)
        assert.ok(src.slice(Math.max(0, i - 400), i).includes('{hasTokenUsage(l) &&'), `${name}的 ${a} 缺 hasTokenUsage 守卫`)
      }
    }
  })

  it('按次行是 `{count} × {单价}/{单位}`（数量后不接单位 token）', () => {
    for (const { name, src } of PAGES) {
      assert.match(src, PER_CALL_LINE, `${name}按次行形状不对`)
    }
  })

  it('fmtPricePerUnit 去尾零顺序无关', () => {
    for (const { name, src } of PAGES) {
      assert.match(src, TRIM_ZERO, `${name}去尾零仍是 /\\.?0+$/`)
    }
  })

  it('两页各自定义 hasTokenUsage（spec §3 阶段约束——非永久禁止抽共享模块）', () => {
    // 这条只在本次改动范围内成立；未来把格式化/判定收进 web/src/lib/ 是正当重构，删掉本用例即可，不要当行为契约。
    for (const { name, src } of PAGES) {
      assert.ok(src.includes('const hasTokenUsage = ('), `${name}缺 hasTokenUsage 判定函数`)
    }
  })
})
