// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// stats 图表回归检查（修复 spec：natural 样条越界 + 动画错误尺寸）。
// 只读源码断言（字符串/正则级别），纯 Node 无依赖；脚本位于 web/scripts/，
// 读取 ../src/...。修复落地前本脚本必须失败（当前代码仍为 natural /
// INITIAL_DIMENSION），修复后应全部通过。
// 用法：pnpm run check:stats-chart（或 node scripts/check-stats-chart.mjs）
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const read = (rel) => {
  try {
    return readFileSync(join(here, rel), 'utf8')
  } catch (e) {
    console.error(`CHECK STATS CHART FAILED: 无法读取 ${rel}（${e.code ?? e.message}）`)
    process.exit(1)
  }
}

const failures = []

function check(name, ok, detail) {
  if (ok) {
    console.log(`  PASS ${name}`)
  } else {
    console.log(`  FAIL ${name}`)
    failures.push({ name, detail })
  }
}

// —— 页面级断言：管理端 + 用户端 Token 图 ——
const PAGES = [
  { label: '管理端 stats.tsx', path: '../src/pages/stats.tsx' },
  { label: '用户端 user/stats.tsx', path: '../src/pages/user/stats.tsx' },
]
const TOKEN_KEYS = ['input', 'cacheRead', 'output', 'cacheWrite']

for (const { label, path } of PAGES) {
  const src = read(path)
  const chartMatch = src.match(/<AreaChart\b[\s\S]*?<\/AreaChart>/)
  check(
    `${label}: Token 图（AreaChart）区块存在`,
    !!chartMatch,
    '未找到 <AreaChart ...>...</AreaChart> 区块'
  )
  if (!chartMatch) continue
  const chart = chartMatch[0]

  const areas = chart.match(/<Area\b[^>]*\/>/g) ?? []
  check(`${label}: Token Area 数量为 4`, areas.length === 4, `实际 ${areas.length} 个`)

  for (const key of TOKEN_KEYS) {
    const tag = areas.find((a) => a.includes(`dataKey="${key}"`))
    check(
      `${label}: Token Area dataKey=${key} 使用 type="linear"`,
      !!tag && tag.includes('type="linear"'),
      tag ? `实际 type 缺失或非 linear：${tag.trim()}` : '未找到该 Area'
    )
  }

  const lines = chart.match(/<Line\b[^>]*\/>/g) ?? []
  const hitRateLine = lines.find((l) => l.includes('dataKey="hitRate"'))
  check(
    `${label}: hitRate Line 使用 type="linear"`,
    !!hitRateLine && hitRateLine.includes('type="linear"'),
    hitRateLine ? `实际：${hitRateLine.trim()}` : '未找到 hitRate Line'
  )

  const naturalCount = (chart.match(/type="natural"/g) ?? []).length
  check(
    `${label}: Token 图内无 type="natural"`,
    naturalCount === 0,
    `发现 ${naturalCount} 处 type="natural"（样条越界回归）`
  )
}

// —— 共享 ChartContainer 断言 ——
const chartContainer = read('../src/components/ui/chart.tsx')
check(
  'ChartContainer: 无 INITIAL_DIMENSION',
  !chartContainer.includes('INITIAL_DIMENSION'),
  '仍存在 INITIAL_DIMENSION（动画错误尺寸回归）'
)
check(
  'ChartContainer: 无 initialDimension=',
  !/initialDimension\s*=/.test(chartContainer),
  '仍存在 initialDimension=（动画错误尺寸回归）'
)

if (failures.length > 0) {
  console.error('\nCHECK STATS CHART FAILED')
  for (const f of failures) {
    console.error(`  - ${f.name}: ${f.detail}`)
  }
  process.exit(1)
}
console.log('\nCHECK STATS CHART OK')
